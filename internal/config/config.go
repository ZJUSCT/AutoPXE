package config

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Listen    Listen `yaml:"listen"`
	DHCP      DHCP   `yaml:"dhcp"`
	IPA       IPA    `yaml:"ipa"`
	Deploy    Deploy `yaml:"deploy"`
	Ironic    Ironic `yaml:"ironic"`
	StateFile string `yaml:"state_file"`
}

type Listen struct {
	Interface  string `yaml:"interface"`
	IP         string `yaml:"ip"`
	DHCPPort   int    `yaml:"dhcp_port"`
	TFTPPort   int    `yaml:"tftp_port"`
	HTTPPort   int    `yaml:"http_port"`
	IronicPort int    `yaml:"ironic_port"`

	ParsedIP net.IP `yaml:"-"`
}

type DHCP struct {
	Subnet    string   `yaml:"subnet"`
	Gateway   string   `yaml:"gateway"`
	DNS       []string `yaml:"dns"`
	LeaseTime string   `yaml:"lease_time"`
	Pool      Pool     `yaml:"pool"`
	Static    []Static `yaml:"static"`

	ParsedSubnet  *net.IPNet      `yaml:"-"`
	ParsedGateway net.IP          `yaml:"-"`
	ParsedDNS     []net.IP        `yaml:"-"`
	ParsedLease   time.Duration   `yaml:"-"`
}

type Pool struct {
	Start string `yaml:"start"`
	End   string `yaml:"end"`

	ParsedStart net.IP `yaml:"-"`
	ParsedEnd   net.IP `yaml:"-"`
}

type Static struct {
	MAC      string   `yaml:"mac"`
	IP       string   `yaml:"ip"`
	Hostname string   `yaml:"hostname,omitempty"`
	Gateway  string   `yaml:"gateway,omitempty"`
	DNS      []string `yaml:"dns,omitempty"`

	ParsedMAC     net.HardwareAddr `yaml:"-"`
	ParsedIP      net.IP           `yaml:"-"`
	ParsedGateway net.IP           `yaml:"-"`
	ParsedDNS     []net.IP         `yaml:"-"`
}

type IPA struct {
	Kernel    string      `yaml:"kernel"`
	Initramfs string      `yaml:"initramfs"`
	Download  IPADownload `yaml:"download"`
}

type IPADownload struct {
	BaseURI  string `yaml:"base_uri"`
	Flavor   string `yaml:"flavor"`
	Branch   string `yaml:"branch"`
	CacheDir string `yaml:"cache_dir"`
}

type Deploy struct {
	Image      string `yaml:"image"`
	ImageType  string `yaml:"image_type"`
	DiskFormat string `yaml:"disk_format"`
	RootMB     int    `yaml:"root_mb"`
	RootGB     int    `yaml:"root_gb,omitempty"`
}

type Ironic struct {
	HeartbeatTimeout int `yaml:"heartbeat_timeout"`
}

func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read config: %w", err)
	}
	var c Config
	if err := yaml.Unmarshal(data, &c); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}
	if err := c.applyDefaults(); err != nil {
		return nil, err
	}
	if err := c.validate(); err != nil {
		return nil, fmt.Errorf("validate: %w", err)
	}
	return &c, nil
}

func (c *Config) applyDefaults() error {
	if c.Listen.DHCPPort == 0 {
		c.Listen.DHCPPort = 67
	}
	if c.Listen.TFTPPort == 0 {
		c.Listen.TFTPPort = 69
	}
	if c.Listen.HTTPPort == 0 {
		c.Listen.HTTPPort = 80
	}
	if c.Listen.IronicPort == 0 {
		c.Listen.IronicPort = 6385
	}
	if c.DHCP.LeaseTime == "" {
		c.DHCP.LeaseTime = "12h"
	}
	if c.Deploy.ImageType == "" {
		c.Deploy.ImageType = "partition"
	}
	if c.Deploy.DiskFormat == "" {
		c.Deploy.DiskFormat = "qcow2"
	}
	if c.Deploy.RootMB == 0 && c.Deploy.RootGB > 0 {
		c.Deploy.RootMB = c.Deploy.RootGB * 1024
	}
	if c.Deploy.RootMB == 0 {
		c.Deploy.RootMB = 51200
	}
	if c.Ironic.HeartbeatTimeout == 0 {
		c.Ironic.HeartbeatTimeout = 300
	}
	if c.StateFile == "" {
		c.StateFile = "/var/lib/autopxe/state.json"
	}
	if c.IPA.Download.BaseURI == "" {
		c.IPA.Download.BaseURI = "https://tarballs.opendev.org/openstack/ironic-python-agent/dib"
	}
	if c.IPA.Download.Flavor == "" {
		c.IPA.Download.Flavor = "centos9"
	}
	if c.IPA.Download.Branch == "" {
		c.IPA.Download.Branch = "master"
	}
	if c.IPA.Download.CacheDir == "" {
		c.IPA.Download.CacheDir = "/var/lib/autopxe/cache"
	}
	return nil
}

func (c *Config) validate() error {
	if c.Listen.Interface == "" {
		return fmt.Errorf("listen.interface required")
	}
	ip := net.ParseIP(c.Listen.IP)
	if ip == nil || ip.To4() == nil {
		return fmt.Errorf("listen.ip invalid IPv4: %q", c.Listen.IP)
	}
	c.Listen.ParsedIP = ip.To4()

	_, ipnet, err := net.ParseCIDR(c.DHCP.Subnet)
	if err != nil {
		return fmt.Errorf("dhcp.subnet invalid: %w", err)
	}
	c.DHCP.ParsedSubnet = ipnet

	gw := net.ParseIP(c.DHCP.Gateway)
	if gw == nil {
		return fmt.Errorf("dhcp.gateway invalid: %q", c.DHCP.Gateway)
	}
	c.DHCP.ParsedGateway = gw.To4()

	for _, d := range c.DHCP.DNS {
		dip := net.ParseIP(d)
		if dip == nil {
			return fmt.Errorf("dhcp.dns invalid: %q", d)
		}
		c.DHCP.ParsedDNS = append(c.DHCP.ParsedDNS, dip.To4())
	}

	dur, err := time.ParseDuration(c.DHCP.LeaseTime)
	if err != nil {
		return fmt.Errorf("dhcp.lease_time invalid: %w", err)
	}
	c.DHCP.ParsedLease = dur

	if c.DHCP.Pool.Start != "" || c.DHCP.Pool.End != "" {
		s := net.ParseIP(c.DHCP.Pool.Start)
		e := net.ParseIP(c.DHCP.Pool.End)
		if s == nil || e == nil {
			return fmt.Errorf("dhcp.pool start/end invalid")
		}
		c.DHCP.Pool.ParsedStart = s.To4()
		c.DHCP.Pool.ParsedEnd = e.To4()
	}

	for i := range c.DHCP.Static {
		s := &c.DHCP.Static[i]
		mac, err := net.ParseMAC(s.MAC)
		if err != nil {
			return fmt.Errorf("dhcp.static[%d].mac invalid: %w", i, err)
		}
		s.MAC = strings.ToLower(mac.String())
		s.ParsedMAC = mac
		sip := net.ParseIP(s.IP)
		if sip == nil {
			return fmt.Errorf("dhcp.static[%d].ip invalid: %q", i, s.IP)
		}
		s.ParsedIP = sip.To4()
		if s.Gateway != "" {
			g := net.ParseIP(s.Gateway)
			if g == nil {
				return fmt.Errorf("dhcp.static[%d].gateway invalid", i)
			}
			s.ParsedGateway = g.To4()
		}
		for _, d := range s.DNS {
			dip := net.ParseIP(d)
			if dip == nil {
				return fmt.Errorf("dhcp.static[%d].dns invalid: %q", i, d)
			}
			s.ParsedDNS = append(s.ParsedDNS, dip.To4())
		}
	}

	if c.Deploy.Image == "" {
		return fmt.Errorf("deploy.image required")
	}
	if c.IPA.Kernel == "" || c.IPA.Initramfs == "" {
		return fmt.Errorf("ipa.kernel and ipa.initramfs required")
	}
	return nil
}
