// Package dns implements an authoritative-with-fallback DNS server. It serves
// four kinds of records, in priority order:
//
//  1. Static records from cfg.DNS.Static (highest priority).
//  2. Auto-registered records for nodes that have completed deployment
//     (state.Tracker says deployed AND a hostname is associated via the DHCP
//     static-binding table); the IP is read from lease.Allocator so dynamic
//     pool nodes also resolve.
//  3. Wildcard suffix matches → forwarded to a per-suffix upstream.
//  4. Default upstream forwarder (cfg.DNS.Upstream) for everything else.
//
// PTR queries get auto-generated reverse records for static + deployed
// addresses; other PTRs fall through to the upstream.
package dns

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"sort"
	"strings"
	"sync"
	"time"

	mdns "github.com/miekg/dns"

	"github.com/ZJUSCT/AutoPXE/internal/config"
	"github.com/ZJUSCT/AutoPXE/internal/lease"
	"github.com/ZJUSCT/AutoPXE/internal/state"
)

type Server struct {
	cfg     *config.Config
	leaser  *lease.Allocator
	tracker *state.Tracker
	logger  *slog.Logger

	staticA   map[string]net.IP // fqdn (lower, with trailing dot) -> IP
	staticPTR map[string]string // arpa (lower) -> fqdn
	wildcards []wildcardEntry   // longest suffix first
	upstreams []string

	clientUDP *mdns.Client
	clientTCP *mdns.Client

	udpSrv *mdns.Server
	tcpSrv *mdns.Server
}

type wildcardEntry struct {
	suffix   string // ".corp.local." (with leading + trailing dot)
	upstream []string
}

func New(cfg *config.Config, leaser *lease.Allocator, tracker *state.Tracker, logger *slog.Logger) *Server {
	s := &Server{
		cfg:       cfg,
		leaser:    leaser,
		tracker:   tracker,
		logger:    logger.With("component", "dns"),
		staticA:   map[string]net.IP{},
		staticPTR: map[string]string{},
		clientUDP: &mdns.Client{Net: "udp", Timeout: 5 * time.Second},
		clientTCP: &mdns.Client{Net: "tcp", Timeout: 5 * time.Second},
		upstreams: append([]string(nil), cfg.DNS.Upstream...),
	}
	for i := range s.upstreams {
		s.upstreams[i] = ensurePort(s.upstreams[i])
	}
	for _, st := range cfg.DNS.Static {
		fqdn := s.fqdn(st.Name)
		s.staticA[fqdn] = st.ParsedIP
		if arpa, err := mdns.ReverseAddr(st.ParsedIP.String()); err == nil {
			s.staticPTR[strings.ToLower(arpa)] = fqdn
		}
	}
	for _, w := range cfg.DNS.Wildcards {
		suf := strings.ToLower(strings.Trim(w.Suffix, "."))
		if suf == "" {
			continue
		}
		ups := append([]string(nil), w.Upstream...)
		for i := range ups {
			ups[i] = ensurePort(ups[i])
		}
		s.wildcards = append(s.wildcards, wildcardEntry{
			suffix:   "." + suf + ".",
			upstream: ups,
		})
	}
	sort.Slice(s.wildcards, func(i, j int) bool {
		return len(s.wildcards[i].suffix) > len(s.wildcards[j].suffix)
	})
	return s
}

func ensurePort(addr string) string {
	if _, _, err := net.SplitHostPort(addr); err == nil {
		return addr
	}
	return net.JoinHostPort(addr, "53")
}

// fqdn converts a name to its canonical form: lower-cased, trailing dot, with
// the configured DNS domain appended if the name has no dot.
func (s *Server) fqdn(name string) string {
	name = strings.ToLower(strings.TrimSuffix(name, "."))
	if s.cfg.DNS.Domain != "" && !strings.Contains(name, ".") {
		name += "." + strings.ToLower(strings.Trim(s.cfg.DNS.Domain, "."))
	}
	return name + "."
}

func (s *Server) Run(ctx context.Context) error {
	if !s.cfg.DNS.Enabled {
		s.logger.Info("dns disabled")
		<-ctx.Done()
		return ctx.Err()
	}

	addr := s.cfg.DNS.Listen
	handler := mdns.HandlerFunc(s.handle)

	s.udpSrv = &mdns.Server{Addr: addr, Net: "udp", Handler: handler}
	s.tcpSrv = &mdns.Server{Addr: addr, Net: "tcp", Handler: handler}

	errCh := make(chan error, 2)
	go func() { errCh <- s.udpSrv.ListenAndServe() }()
	go func() { errCh <- s.tcpSrv.ListenAndServe() }()

	s.logger.Info("dns server listening",
		"addr", addr,
		"static", len(s.staticA),
		"wildcards", len(s.wildcards),
		"upstreams", len(s.upstreams),
	)

	select {
	case <-ctx.Done():
		_ = s.udpSrv.Shutdown()
		_ = s.tcpSrv.Shutdown()
		return ctx.Err()
	case err := <-errCh:
		_ = s.udpSrv.Shutdown()
		_ = s.tcpSrv.Shutdown()
		return err
	}
}

func (s *Server) handle(w mdns.ResponseWriter, req *mdns.Msg) {
	if len(req.Question) == 0 {
		_ = w.WriteMsg(refused(req))
		return
	}
	q := req.Question[0]
	qname := strings.ToLower(q.Name)

	resp := new(mdns.Msg)
	resp.SetReply(req)
	resp.Authoritative = false
	resp.RecursionAvailable = true

	switch q.Qtype {
	case mdns.TypeA:
		if ip, ok := s.localA(qname); ok {
			resp.Authoritative = true
			resp.Answer = append(resp.Answer, &mdns.A{
				Hdr: mdns.RR_Header{Name: q.Name, Rrtype: mdns.TypeA, Class: mdns.ClassINET, Ttl: s.cfg.DNS.TTL},
				A:   ip.To4(),
			})
			s.logger.Debug("dns local A", "name", qname, "ip", ip.String())
			_ = w.WriteMsg(resp)
			return
		}
	case mdns.TypeAAAA:
		// We have no IPv6 records of our own; let upstream answer (or NODATA).
	case mdns.TypePTR:
		if name, ok := s.localPTR(qname); ok {
			resp.Authoritative = true
			resp.Answer = append(resp.Answer, &mdns.PTR{
				Hdr: mdns.RR_Header{Name: q.Name, Rrtype: mdns.TypePTR, Class: mdns.ClassINET, Ttl: s.cfg.DNS.TTL},
				Ptr: name,
			})
			s.logger.Debug("dns local PTR", "name", qname, "target", name)
			_ = w.WriteMsg(resp)
			return
		}
	}

	// Forward to upstream (wildcard match first, then default).
	upstreams := s.upstreamsFor(qname)
	if len(upstreams) == 0 {
		// No upstream configured: NXDOMAIN for unmatched.
		resp.Rcode = mdns.RcodeNameError
		_ = w.WriteMsg(resp)
		return
	}
	if err := s.forward(w, req, upstreams); err != nil {
		s.logger.Warn("dns forward failed", "name", qname, "err", err.Error())
		resp.Rcode = mdns.RcodeServerFailure
		_ = w.WriteMsg(resp)
	}
}

func (s *Server) localA(qname string) (net.IP, bool) {
	if ip, ok := s.staticA[qname]; ok {
		return ip, true
	}
	if s.cfg.DNS.RegisterDeployed != nil && *s.cfg.DNS.RegisterDeployed {
		if ip, ok := s.deployedA(qname); ok {
			return ip, true
		}
	}
	return nil, false
}

func (s *Server) deployedA(qname string) (net.IP, bool) {
	for _, st := range s.cfg.DHCP.Static {
		if st.Hostname == "" {
			continue
		}
		if s.fqdn(st.Hostname) != qname {
			continue
		}
		if !s.tracker.IsDeployed(st.MAC) {
			continue
		}
		l := s.leaser.Lookup(st.MAC)
		if l == nil {
			continue
		}
		return l.IP, true
	}
	return nil, false
}

func (s *Server) localPTR(qname string) (string, bool) {
	if name, ok := s.staticPTR[qname]; ok {
		return name, true
	}
	if s.cfg.DNS.RegisterDeployed == nil || !*s.cfg.DNS.RegisterDeployed {
		return "", false
	}
	for _, st := range s.cfg.DHCP.Static {
		if st.Hostname == "" {
			continue
		}
		if !s.tracker.IsDeployed(st.MAC) {
			continue
		}
		l := s.leaser.Lookup(st.MAC)
		if l == nil {
			continue
		}
		arpa, err := mdns.ReverseAddr(l.IP.String())
		if err != nil {
			continue
		}
		if strings.ToLower(arpa) == qname {
			return s.fqdn(st.Hostname), true
		}
	}
	return "", false
}

func (s *Server) upstreamsFor(qname string) []string {
	for _, w := range s.wildcards {
		if strings.HasSuffix(qname, w.suffix) || strings.TrimSuffix(qname, ".") == strings.Trim(w.suffix, ".") {
			return w.upstream
		}
	}
	return s.upstreams
}

func (s *Server) forward(w mdns.ResponseWriter, req *mdns.Msg, upstreams []string) error {
	tcp := isTCPConn(w.RemoteAddr())
	client := s.clientUDP
	if tcp {
		client = s.clientTCP
	}

	var lastErr error
	for _, u := range upstreams {
		resp, _, err := client.Exchange(req, u)
		if err != nil {
			lastErr = err
			continue
		}
		// If upstream truncated a UDP response, retry over TCP.
		if !tcp && resp != nil && resp.Truncated {
			if r2, _, err2 := s.clientTCP.Exchange(req, u); err2 == nil {
				resp = r2
			} else {
				lastErr = err2
			}
		}
		if resp == nil {
			continue
		}
		resp.Id = req.Id
		return w.WriteMsg(resp)
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no upstream answered")
	}
	return lastErr
}

func isTCPConn(addr net.Addr) bool {
	_, ok := addr.(*net.TCPAddr)
	return ok
}

func refused(req *mdns.Msg) *mdns.Msg {
	r := new(mdns.Msg)
	r.SetReply(req)
	r.Rcode = mdns.RcodeRefused
	return r
}

// Touch is a no-op kept so future test helpers can validate sync handling.
var _ = sync.Mutex{}
