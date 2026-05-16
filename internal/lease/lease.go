package lease

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/ZJUSCT/AutoPXE/internal/config"
)

const currentVersion = 1

type Lease struct {
	MAC      string
	IP       net.IP
	Gateway  net.IP
	DNS      []net.IP
	Hostname string
	Expires  time.Time
	Static   bool
}

type fileFormat struct {
	Version  int                       `json:"version"`
	Leases   map[string]leaseRecord    `json:"leases"`
	Declined map[string]declinedRecord `json:"declined,omitempty"`
}

type leaseRecord struct {
	IP      string    `json:"ip"`
	Expires time.Time `json:"expires"`
}

type declinedRecord struct {
	MAC       string    `json:"mac,omitempty"`
	ExpiresAt time.Time `json:"expires_at"`
}

type declinedLease struct {
	MAC       string
	ExpiresAt time.Time
}

type Allocator struct {
	mu sync.Mutex

	leaseFile string
	now       func() time.Time

	defaultGateway net.IP
	defaultDNS     []net.IP
	leaseTime      time.Duration

	static map[string]*Lease // MAC -> static lease (no expiry)

	hasPool            bool
	poolStart, poolEnd uint32
	dynamic            map[string]*Lease // MAC -> dynamic lease
	byIP               map[uint32]string // IP -> MAC for collision check
	declined           map[uint32]declinedLease
}

func NewAllocator(c *config.Config) (*Allocator, error) {
	a := &Allocator{
		leaseFile:      c.DHCP.LeaseFile,
		now:            time.Now,
		defaultGateway: c.DHCP.ParsedGateway,
		defaultDNS:     append([]net.IP(nil), c.DHCP.ParsedDNS...),
		leaseTime:      c.DHCP.ParsedLease,
		static:         make(map[string]*Lease),
		dynamic:        make(map[string]*Lease),
		byIP:           make(map[uint32]string),
		declined:       make(map[uint32]declinedLease),
	}

	for _, s := range c.DHCP.Static {
		gw := s.ParsedGateway
		if gw == nil {
			gw = a.defaultGateway
		}
		dns := s.ParsedDNS
		if len(dns) == 0 {
			dns = a.defaultDNS
		}
		mac := strings.ToLower(s.MAC)
		a.static[mac] = &Lease{
			MAC:      mac,
			IP:       s.ParsedIP,
			Gateway:  gw,
			DNS:      dns,
			Hostname: s.Hostname,
			Static:   true,
		}
	}

	if c.DHCP.Pool.ParsedStart != nil && c.DHCP.Pool.ParsedEnd != nil {
		a.poolStart = ipToU32(c.DHCP.Pool.ParsedStart)
		a.poolEnd = ipToU32(c.DHCP.Pool.ParsedEnd)
		if a.poolStart > a.poolEnd {
			return nil, fmt.Errorf("dhcp.pool start > end")
		}
		a.hasPool = true
	}

	if err := a.load(); err != nil {
		return nil, err
	}
	return a, nil
}

func ClearLeaseFile(path string) error {
	if path == "" {
		return nil
	}
	f := fileFormat{
		Version:  currentVersion,
		Leases:   map[string]leaseRecord{},
		Declined: map[string]declinedRecord{},
	}
	return writeFile(path, f)
}

func canon(mac string) string {
	if hw, err := net.ParseMAC(mac); err == nil {
		return strings.ToLower(hw.String())
	}
	return strings.ToLower(strings.TrimSpace(mac))
}

func ipToU32(ip net.IP) uint32 {
	ip = ip.To4()
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}

func u32ToIP(v uint32) net.IP {
	return net.IPv4(byte(v>>24), byte(v>>16), byte(v>>8), byte(v)).To4()
}

// Allocate returns the lease that should be served to the given MAC.
// Static bindings always win; otherwise an existing dynamic lease is reused
// (extending its expiry); otherwise a fresh IP is picked from the pool.
// Dynamic leases are persisted so restart cannot make autopxe re-issue an
// address that is still occupied from the server's point of view.
func (a *Allocator) Allocate(mac string) (*Lease, error) {
	m := canon(mac)
	if m == "" {
		return nil, fmt.Errorf("empty mac")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	now := a.now()
	if l, ok := a.static[m]; ok {
		l.Expires = now.Add(a.leaseTime)
		return l, nil
	}

	a.reapExpiredLocked(now)
	if l, ok := a.dynamic[m]; ok {
		l.Expires = now.Add(a.leaseTime)
		return l, a.saveLocked()
	}

	if !a.hasPool {
		return nil, fmt.Errorf("no static binding for %s and pool not configured", m)
	}

	staticIPs := a.staticIPSetLocked()
	for v := a.poolStart; ; v++ {
		if _, taken := a.byIP[v]; taken {
			if v == a.poolEnd {
				break
			}
			continue
		}
		if _, taken := staticIPs[v]; taken {
			if v == a.poolEnd {
				break
			}
			continue
		}
		if d, declined := a.declined[v]; declined && d.ExpiresAt.After(now) {
			if v == a.poolEnd {
				break
			}
			continue
		}
		l := &Lease{
			MAC:     m,
			IP:      u32ToIP(v),
			Gateway: a.defaultGateway,
			DNS:     a.defaultDNS,
			Expires: now.Add(a.leaseTime),
		}
		a.dynamic[m] = l
		a.byIP[v] = m
		return l, a.saveLocked()
	}
	return nil, fmt.Errorf("dhcp pool exhausted")
}

// Lookup returns the existing lease for a MAC without allocating.
func (a *Allocator) Lookup(mac string) *Lease {
	m := canon(mac)
	a.mu.Lock()
	defer a.mu.Unlock()
	if l, ok := a.static[m]; ok {
		return l
	}
	if l, ok := a.dynamic[m]; ok && l.Expires.After(a.now()) {
		return l
	}
	return nil
}

// Release removes a dynamic lease when a DHCPRELEASE is received. Static
// bindings are configuration, not pool state, so they are intentionally kept.
func (a *Allocator) Release(mac string, ip net.IP) error {
	m := canon(mac)
	if m == "" {
		return fmt.Errorf("empty mac")
	}
	a.mu.Lock()
	defer a.mu.Unlock()

	l, ok := a.dynamic[m]
	if !ok {
		return nil
	}
	if ip4 := ip.To4(); ip4 != nil && !l.IP.Equal(ip4) {
		return nil
	}
	delete(a.byIP, ipToU32(l.IP))
	delete(a.dynamic, m)
	return a.saveLocked()
}

// Decline marks an address as occupied after a DHCPDECLINE. The address is
// held out of the dynamic pool for one lease interval and persisted, so the
// same conflict is not immediately offered again after restart.
func (a *Allocator) Decline(mac string, ip net.IP) error {
	m := canon(mac)
	ip4 := ip.To4()
	if ip4 == nil {
		return fmt.Errorf("invalid declined ip")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	v := ipToU32(ip4)
	if owner, ok := a.byIP[v]; ok {
		delete(a.dynamic, owner)
		delete(a.byIP, v)
	}
	if _, static := a.staticIPSetLocked()[v]; !static && a.inPoolLocked(v) {
		a.declined[v] = declinedLease{
			MAC:       m,
			ExpiresAt: a.now().Add(a.leaseTime),
		}
	}
	return a.saveLocked()
}

// ClearDynamic removes all server-side dynamic leases and declined/conflict
// records. It does not alter static bindings from the config file.
func (a *Allocator) ClearDynamic() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.dynamic = make(map[string]*Lease)
	a.byIP = make(map[uint32]string)
	a.declined = make(map[uint32]declinedLease)
	return a.saveLocked()
}

func (a *Allocator) load() error {
	if a.leaseFile == "" {
		return nil
	}
	raw, err := os.ReadFile(a.leaseFile)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read lease file: %w", err)
	}
	var f fileFormat
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("parse lease file %s: %w", a.leaseFile, err)
	}
	if f.Leases == nil {
		f.Leases = map[string]leaseRecord{}
	}
	if f.Declined == nil {
		f.Declined = map[string]declinedRecord{}
	}

	now := a.now()
	staticIPs := a.staticIPSetLocked()
	for mac, rec := range f.Leases {
		m := canon(mac)
		ip := net.ParseIP(rec.IP).To4()
		if m == "" || ip == nil || !rec.Expires.After(now) {
			continue
		}
		if _, staticMAC := a.static[m]; staticMAC {
			continue
		}
		v := ipToU32(ip)
		if !a.inPoolLocked(v) {
			continue
		}
		if _, static := staticIPs[v]; static {
			continue
		}
		if _, taken := a.byIP[v]; taken {
			continue
		}
		l := &Lease{
			MAC:     m,
			IP:      ip,
			Gateway: a.defaultGateway,
			DNS:     a.defaultDNS,
			Expires: rec.Expires,
		}
		a.dynamic[m] = l
		a.byIP[v] = m
	}
	for ipStr, rec := range f.Declined {
		ip := net.ParseIP(ipStr).To4()
		if ip == nil || !rec.ExpiresAt.After(now) {
			continue
		}
		v := ipToU32(ip)
		if !a.inPoolLocked(v) {
			continue
		}
		if _, static := staticIPs[v]; static {
			continue
		}
		a.declined[v] = declinedLease{
			MAC:       canon(rec.MAC),
			ExpiresAt: rec.ExpiresAt,
		}
	}
	return nil
}

func (a *Allocator) saveLocked() error {
	if a.leaseFile == "" {
		return nil
	}
	f := fileFormat{
		Version:  currentVersion,
		Leases:   make(map[string]leaseRecord, len(a.dynamic)),
		Declined: make(map[string]declinedRecord, len(a.declined)),
	}
	for mac, l := range a.dynamic {
		f.Leases[mac] = leaseRecord{
			IP:      l.IP.String(),
			Expires: l.Expires,
		}
	}
	for v, d := range a.declined {
		f.Declined[u32ToIP(v).String()] = declinedRecord{
			MAC:       d.MAC,
			ExpiresAt: d.ExpiresAt,
		}
	}
	return writeFile(a.leaseFile, f)
}

func writeFile(path string, f fileFormat) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("mkdir lease dir: %w", err)
	}
	f.Version = currentVersion
	if f.Leases == nil {
		f.Leases = map[string]leaseRecord{}
	}
	raw, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (a *Allocator) reapExpiredLocked(now time.Time) {
	for mac, l := range a.dynamic {
		if !l.Expires.After(now) {
			delete(a.byIP, ipToU32(l.IP))
			delete(a.dynamic, mac)
		}
	}
	for ip, d := range a.declined {
		if !d.ExpiresAt.After(now) {
			delete(a.declined, ip)
		}
	}
}

func (a *Allocator) staticIPSetLocked() map[uint32]struct{} {
	staticIPs := make(map[uint32]struct{}, len(a.static))
	for _, l := range a.static {
		staticIPs[ipToU32(l.IP)] = struct{}{}
	}
	return staticIPs
}

func (a *Allocator) inPoolLocked(v uint32) bool {
	return a.hasPool && v >= a.poolStart && v <= a.poolEnd
}
