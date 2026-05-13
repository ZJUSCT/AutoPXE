package lease

import (
	"fmt"
	"net"
	"strings"
	"sync"
	"time"

	"autopxe/internal/config"
)

type Lease struct {
	MAC      string
	IP       net.IP
	Gateway  net.IP
	DNS      []net.IP
	Hostname string
	Expires  time.Time
	Static   bool
}

type Allocator struct {
	mu sync.Mutex

	defaultGateway net.IP
	defaultDNS     []net.IP
	leaseTime      time.Duration

	static map[string]*Lease // MAC -> static lease (no expiry)

	poolStart, poolEnd uint32
	dynamic            map[string]*Lease // MAC -> dynamic lease
	byIP               map[uint32]string // IP -> MAC for collision check
}

func NewAllocator(c *config.Config) (*Allocator, error) {
	a := &Allocator{
		defaultGateway: c.DHCP.ParsedGateway,
		defaultDNS:     append([]net.IP(nil), c.DHCP.ParsedDNS...),
		leaseTime:      c.DHCP.ParsedLease,
		static:         make(map[string]*Lease),
		dynamic:        make(map[string]*Lease),
		byIP:           make(map[uint32]string),
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
	}
	return a, nil
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
// The returned lease is safe to read; the underlying entry is updated in place.
func (a *Allocator) Allocate(mac string) (*Lease, error) {
	m := canon(mac)
	if m == "" {
		return nil, fmt.Errorf("empty mac")
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if l, ok := a.static[m]; ok {
		l.Expires = time.Now().Add(a.leaseTime)
		return l, nil
	}

	if l, ok := a.dynamic[m]; ok {
		l.Expires = time.Now().Add(a.leaseTime)
		return l, nil
	}

	if a.poolStart == 0 || a.poolEnd == 0 {
		return nil, fmt.Errorf("no static binding for %s and pool not configured", m)
	}

	// Reap expired dynamic leases first
	now := time.Now()
	for k, l := range a.dynamic {
		if l.Expires.Before(now) {
			delete(a.byIP, ipToU32(l.IP))
			delete(a.dynamic, k)
		}
	}
	// Skip IPs claimed by static bindings
	staticIPs := make(map[uint32]struct{}, len(a.static))
	for _, l := range a.static {
		staticIPs[ipToU32(l.IP)] = struct{}{}
	}
	for v := a.poolStart; v <= a.poolEnd; v++ {
		if _, taken := a.byIP[v]; taken {
			continue
		}
		if _, taken := staticIPs[v]; taken {
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
		return l, nil
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
	if l, ok := a.dynamic[m]; ok {
		return l
	}
	return nil
}
