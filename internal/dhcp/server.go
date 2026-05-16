package dhcp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"strings"

	"github.com/insomniacslk/dhcp/dhcpv4"
	"github.com/insomniacslk/dhcp/dhcpv4/server4"

	"github.com/ZJUSCT/AutoPXE/internal/config"
	"github.com/ZJUSCT/AutoPXE/internal/lease"
	"github.com/ZJUSCT/AutoPXE/internal/pxe"
	"github.com/ZJUSCT/AutoPXE/internal/state"
)

type Server struct {
	cfg     *config.Config
	leaser  *lease.Allocator
	tracker *state.Tracker
	logger  *slog.Logger
	srv     *server4.Server
}

func New(cfg *config.Config, leaser *lease.Allocator, tracker *state.Tracker, logger *slog.Logger) *Server {
	return &Server{
		cfg:     cfg,
		leaser:  leaser,
		tracker: tracker,
		logger:  logger.With("component", "dhcp"),
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
	case dhcpv4.MessageTypeRelease:
		if err := s.leaser.Release(mac, req.ClientIPAddr); err != nil {
			s.logger.Warn("release failed", "mac", mac, "ip", req.ClientIPAddr.String(), "err", err.Error())
		} else {
			s.logger.Info("lease released", "mac", mac, "ip", req.ClientIPAddr.String())
		}
		return
	case dhcpv4.MessageTypeDecline:
		ip := requestedOrClientIP(req)
		if ip == nil {
			s.logger.Warn("decline without ip", "mac", mac)
			return
		}
		if err := s.leaser.Decline(mac, ip); err != nil {
			s.logger.Warn("decline failed", "mac", mac, "ip", ip.String(), "err", err.Error())
		} else {
			s.logger.Warn("lease declined; address marked occupied", "mac", mac, "ip", ip.String())
		}
		return
	case dhcpv4.MessageTypeInform:
		return
	default:
		return
	}

	leaseEntry, err := s.leaser.Allocate(mac)
	if err != nil {
		s.logger.Warn("lease allocation failed", "mac", mac, "err", err.Error())
		return
	}

	// Already-deployed gate: if this MAC has a deployment record, treat the
	// request as a plain (non-PXE) DHCP exchange — no PXE markers, no boot
	// file. The firmware's PXE attempt will fail and the machine falls
	// through to the next entry in its boot order (typically the local disk
	// it was just deployed to).
	deployed := s.tracker != nil && s.tracker.IsDeployed(mac)
	if deployed {
		s.logger.Info("mac already deployed; suppressing pxe", "mac", mac)
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
	if s.cfg.DHCP.DomainName != "" {
		mods = append(mods, dhcpv4.WithOption(dhcpv4.OptDomainName(s.cfg.DHCP.DomainName)))
	}
	if len(s.cfg.DHCP.SearchDomains) > 0 {
		mods = append(mods, dhcpv4.WithDomainSearchList(s.cfg.DHCP.SearchDomains...))
	}

	// Always advertise option 60 = "PXEClient" on replies to any client that
	// announced a PXE architecture (option 93). Many BIOS / UEFI PXE ROMs
	// drop OFFERs that lack this marker even when every other PXE field is
	// correct (the IP stack happily accepts the lease, but the PXE stack
	// reports "no offer received"). Also needed for clients that omit option
	// 60 in their request or send a non-"PXEClient" vendor-class.
	if arch != pxe.ArchUnknown && !deployed {
		mods = append(mods, dhcpv4.WithOption(dhcpv4.OptClassIdentifier("PXEClient")))

		// Option 43 (Vendor-Specific Information) with PXE sub-option 6
		// (PXE_DISCOVERY_CONTROL) = 8 tells the client "use the boot file
		// from the BOOTP `file` field directly, do not perform broadcast or
		// multicast PXE discovery". Many UEFI PXE ROMs request option 43
		// (it appears in their Parameter-Request List) and silently reject
		// the OFFER if it's missing, even when option 60, siaddr and option
		// 67 are all correct. The trailing 0xff is the sub-option END
		// marker per the PXE spec.
		mods = append(mods, dhcpv4.WithGeneric(
			dhcpv4.OptionVendorSpecificInformation,
			[]byte{0x06, 0x01, 0x08, 0xff},
		))
	}

	reply, err := dhcpv4.NewReplyFromRequest(req, mods...)
	if err != nil {
		s.logger.Error("build reply", "err", err.Error())
		return
	}

	// Boot file selection. We only set boot fields when this looks like a
	// PXE / iPXE client; "ordinary" DHCP clients (e.g. the booted IPA
	// initramfs requesting an in-band IP, or any other non-PXE host on the
	// provisioning network) get a plain DHCP lease with no boot fields so
	// they can come up normally.
	reply.ServerHostName = s.cfg.Listen.ParsedIP.String()

	bootURL, bootFile, ok := s.selectBoot(arch, isIPXE, mac)
	switch {
	case deployed:
		// hand out the lease, no boot fields
	case isIPXE && ok:
		// iPXE re-DHCP: hand back an HTTP URL, no TFTP filename. iPXE will
		// chainload `boot.ipxe` from this URL.
		reply.BootFileName = bootURL
		reply.UpdateOption(dhcpv4.OptBootFileName(bootURL))
		reply.UpdateOption(dhcpv4.OptTFTPServerName(s.cfg.Listen.ParsedIP.String()))
	case !isIPXE && ok:
		// Firmware DHCP: hand back the iPXE binary via TFTP.
		reply.BootFileName = bootFile
		reply.UpdateOption(dhcpv4.OptBootFileName(bootFile))
		reply.UpdateOption(dhcpv4.OptTFTPServerName(s.cfg.Listen.ParsedIP.String()))
	default:
		// Non-PXE DHCP: just hand out the lease (IP / mask / gateway / DNS).
		s.logger.Info("non-pxe lease only", "mac", mac, "arch", arch.String(), "ipxe", isIPXE)
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

func requestedOrClientIP(req *dhcpv4.DHCPv4) net.IP {
	if ip := req.RequestedIPAddress(); ip != nil {
		if ip4 := ip.To4(); ip4 != nil && !ip4.Equal(net.IPv4zero) {
			return ip4
		}
	}
	if ip := req.ClientIPAddr; ip != nil {
		if ip4 := ip.To4(); ip4 != nil && !ip4.Equal(net.IPv4zero) {
			return ip4
		}
	}
	return nil
}
