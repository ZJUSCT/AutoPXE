package dhcp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"

	"autopxe/internal/config"
	"autopxe/internal/lease"
	"autopxe/internal/pxe"
)

type Server struct {
	cfg    *config.Config
	leaser *lease.Allocator
	logger *slog.Logger
	srv    *server4.Server
}

func New(cfg *config.Config, leaser *lease.Allocator, logger *slog.Logger) *Server {
	return &Server{
		cfg:    cfg,
		leaser: leaser,
		logger: logger.With("component", "dhcp"),
	}
}

func (s *Server) Run(ctx context.Context) error {
	laddr := &net.UDPAddr{IP: net.IPv4zero, Port: s.cfg.Listen.DHCPPort}
	srv, err := server4.NewServer(s.cfg.Listen.Interface, laddr, s.handle)
	if err != nil {
		return fmt.Errorf("dhcp listen: %w", err)
	}
	s.srv = srv

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve() }()

	s.logger.Info("dhcp server listening", "iface", s.cfg.Listen.Interface, "addr", laddr.String())

	select {
	case <-ctx.Done():
		_ = srv.Close()
		<-errCh
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func (s *Server) handle(conn net.PacketConn, peer net.Addr, req *dhcpv4.DHCPv4) {
	mt := req.MessageType()
	mac := strings.ToLower(req.ClientHWAddr.String())
	arch := pxe.DetectArch(req)
	isIPXE := pxe.IsIPXE(req)

	s.logger.Info("dhcp request",
		"type", mt.String(),
		"mac", mac,
		"arch", arch.String(),
		"ipxe", isIPXE,
		"xid", req.TransactionID.String(),
	)

	switch mt {
	case dhcpv4.MessageTypeDiscover, dhcpv4.MessageTypeRequest:
		// proceed
	case dhcpv4.MessageTypeRelease, dhcpv4.MessageTypeDecline, dhcpv4.MessageTypeInform:
		return
	default:
		return
	}

	leaseEntry, err := s.leaser.Allocate(mac)
	if err != nil {
		s.logger.Warn("lease allocation failed", "mac", mac, "err", err.Error())
		return
	}

	replyType := dhcpv4.MessageTypeOffer
	if mt == dhcpv4.MessageTypeRequest {
		replyType = dhcpv4.MessageTypeAck
	}

	mods := []dhcpv4.Modifier{
		dhcpv4.WithReply(req),
		dhcpv4.WithMessageType(replyType),
		dhcpv4.WithYourIP(leaseEntry.IP),
		dhcpv4.WithServerIP(s.cfg.Listen.ParsedIP),
		dhcpv4.WithNetmask(s.cfg.DHCP.ParsedSubnet.Mask),
		dhcpv4.WithLeaseTime(uint32(s.cfg.DHCP.ParsedLease.Seconds())),
		dhcpv4.WithOption(dhcpv4.OptServerIdentifier(s.cfg.Listen.ParsedIP)),
		dhcpv4.WithOption(dhcpv4.OptRouter(leaseEntry.Gateway)),
	}
	if len(leaseEntry.DNS) > 0 {
		mods = append(mods, dhcpv4.WithOption(dhcpv4.OptDNS(leaseEntry.DNS...)))
	}
	if leaseEntry.Hostname != "" {
		mods = append(mods, dhcpv4.WithOption(dhcpv4.OptHostName(leaseEntry.Hostname)))
	}

	// Always advertise option 60 = "PXEClient" on replies to any client that
	// announced a PXE architecture (option 93). Many BIOS / UEFI PXE ROMs
	// drop OFFERs that lack this marker even when every other PXE field is
	// correct (the IP stack happily accepts the lease, but the PXE stack
	// reports "no offer received"). Also needed for clients that omit option
	// 60 in their request or send a non-"PXEClient" vendor-class.
	if arch != pxe.ArchUnknown {
		mods = append(mods, dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient")))
	}

	reply, err := dhcpv4.NewReplyFromRequest(req, mods...)
	if err != nil {
		s.logger.Error("build reply", "err", err.Error())
		return
	}

	// Boot file selection — belt-and-suspenders: BOOTP siaddr/file fields plus
	// options 66 / 67. The same set is sent on OFFER and ACK.
	reply.ServerHostName = s.cfg.Listen.ParsedIP.String()
	reply.UpdateOption(dhcpv4.OptTFTPServerName(s.cfg.Listen.ParsedIP.String()))

	bootURL, bootFile, ok := s.selectBoot(arch, isIPXE, mac)
	switch {
	case isIPXE && ok:
		// iPXE re-DHCP: hand back an HTTP URL, no TFTP filename. iPXE will
		// chainload `boot.ipxe` from this URL.
		reply.BootFileName = bootURL
		reply.UpdateOption(dhcpv4.OptBootFileName(bootURL))
	case !isIPXE && ok:
		// Firmware DHCP: hand back the iPXE binary via TFTP.
		reply.BootFileName = bootFile
		reply.UpdateOption(dhcpv4.OptBootFileName(bootFile))
	default:
		s.logger.Warn("unsupported PXE arch, refusing boot", "mac", mac, "arch", arch.String())
		return
	}

	if _, err := conn.WriteTo(reply.ToBytes(), peer); err != nil {
		s.logger.Error("write reply", "err", err.Error())
		return
	}

	s.logger.Info("dhcp reply sent",
		"type", replyType.String(),
		"mac", mac,
		"yiaddr", leaseEntry.IP.String(),
		"siaddr", s.cfg.Listen.ParsedIP.String(),
		"bootfile", reply.BootFileName,
	)
}

func (s *Server) selectBoot(arch pxe.Arch, isIPXE bool, mac string) (bootURL, bootFile string, ok bool) {
	if isIPXE {
		url := fmt.Sprintf("http://%s:%d/boot.ipxe?mac=%s",
			s.cfg.Listen.ParsedIP.String(), s.cfg.Listen.HTTPPort, mac)
		return url, "", true
	}
	if name, supported := pxe.FirmwareBootfile(arch); supported {
		return "", name, true
	}
	return "", "", false
}
