package lease

import (
	"encoding/json"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ZJUSCT/AutoPXE/internal/config"
)

func TestAllocatorPersistsDynamicLeases(t *testing.T) {
	cfg := testConfig(t, filepath.Join(t.TempDir(), "leases.json"))

	a, err := NewAllocator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	l1, err := a.Allocate("aa:bb:cc:dd:ee:01")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := l1.IP.String(), "192.168.100.100"; got != want {
		t.Fatalf("first lease ip = %s, want %s", got, want)
	}

	reloaded, err := NewAllocator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	l1Again, err := reloaded.Allocate("aa:bb:cc:dd:ee:01")
	if err != nil {
		t.Fatal(err)
	}
	if !l1Again.IP.Equal(l1.IP) {
		t.Fatalf("reloaded lease ip = %s, want %s", l1Again.IP, l1.IP)
	}
	l2, err := reloaded.Allocate("aa:bb:cc:dd:ee:02")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := l2.IP.String(), "192.168.100.101"; got != want {
		t.Fatalf("second lease ip = %s, want %s", got, want)
	}
}

func TestAllocatorClearDynamicLeaseFile(t *testing.T) {
	cfg := testConfig(t, filepath.Join(t.TempDir(), "leases.json"))

	a, err := NewAllocator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.Allocate("aa:bb:cc:dd:ee:01"); err != nil {
		t.Fatal(err)
	}
	if err := a.Decline("aa:bb:cc:dd:ee:02", mustIP("192.168.100.101")); err != nil {
		t.Fatal(err)
	}
	if err := a.ClearDynamic(); err != nil {
		t.Fatal(err)
	}

	raw, err := os.ReadFile(cfg.DHCP.LeaseFile)
	if err != nil {
		t.Fatal(err)
	}
	var stored fileFormat
	if err := json.Unmarshal(raw, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.Leases) != 0 || len(stored.Declined) != 0 {
		t.Fatalf("lease file not cleared: %+v", stored)
	}

	reloaded, err := NewAllocator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	l, err := reloaded.Allocate("aa:bb:cc:dd:ee:03")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := l.IP.String(), "192.168.100.100"; got != want {
		t.Fatalf("lease after clear = %s, want %s", got, want)
	}
}

func TestAllocatorSkipsDeclinedAddressAfterRestart(t *testing.T) {
	cfg := testConfig(t, filepath.Join(t.TempDir(), "leases.json"))

	a, err := NewAllocator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Decline("aa:bb:cc:dd:ee:01", mustIP("192.168.100.100")); err != nil {
		t.Fatal(err)
	}

	reloaded, err := NewAllocator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	l, err := reloaded.Allocate("aa:bb:cc:dd:ee:02")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := l.IP.String(), "192.168.100.101"; got != want {
		t.Fatalf("lease after declined address = %s, want %s", got, want)
	}
}

func TestAllocatorSkipsStaticAddressInPool(t *testing.T) {
	cfg := testConfig(t, filepath.Join(t.TempDir(), "leases.json"))
	cfg.DHCP.Static = []config.Static{{
		MAC:      "aa:bb:cc:dd:ee:ff",
		IP:       "192.168.100.100",
		ParsedIP: mustIP("192.168.100.100"),
	}}

	a, err := NewAllocator(cfg)
	if err != nil {
		t.Fatal(err)
	}
	l, err := a.Allocate("aa:bb:cc:dd:ee:01")
	if err != nil {
		t.Fatal(err)
	}
	if got, want := l.IP.String(), "192.168.100.101"; got != want {
		t.Fatalf("dynamic lease ip = %s, want %s", got, want)
	}
}

func testConfig(t *testing.T, leaseFile string) *config.Config {
	t.Helper()
	return &config.Config{
		DHCP: config.DHCP{
			ParsedGateway: mustIP("192.168.100.1"),
			ParsedDNS:     []net.IP{mustIP("8.8.8.8")},
			ParsedLease:   time.Hour,
			LeaseFile:     leaseFile,
			Pool: config.Pool{
				ParsedStart: mustIP("192.168.100.100"),
				ParsedEnd:   mustIP("192.168.100.102"),
			},
		},
	}
}

func mustIP(s string) net.IP {
	ip := net.ParseIP(s).To4()
	if ip == nil {
		panic(s)
	}
	return ip
}
