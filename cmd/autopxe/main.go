package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"golang.org/x/sync/errgroup"

	"autopxe/internal/assets"
	"autopxe/internal/config"
	"autopxe/internal/dhcp"
	"autopxe/internal/httpsrv"
	"autopxe/internal/ipadl"
	"autopxe/internal/ironic"
	"autopxe/internal/lease"
	"autopxe/internal/node"
	"autopxe/internal/tftp"
)

func main() {
	cfgPath := flag.String("config", "/etc/autopxe/config.yaml", "path to config file")
	logLevel := flag.String("log-level", "info", "log level (debug, info, warn, error)")
	flag.Parse()

	logger := newLogger(*logLevel)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Error("load config", "err", err.Error())
		os.Exit(1)
	}
	logger.Info("config loaded",
		"interface", cfg.Listen.Interface,
		"ip", cfg.Listen.IP,
		"static_bindings", len(cfg.DHCP.Static),
	)

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := ipadl.EnsureArtifacts(ctx, cfg, logger); err != nil {
		logger.Error("ensure ipa artifacts", "err", err.Error())
		os.Exit(1)
	}

	leaser, err := lease.NewAllocator(cfg)
	if err != nil {
		logger.Error("lease allocator", "err", err.Error())
		os.Exit(1)
	}
	store := node.NewStore()

	httpServer := httpsrv.New(cfg, logger)
	tftpServer := tftp.New(cfg, assets.IPXE(), logger)
	dhcpServer := dhcp.New(cfg, leaser, logger)
	ironicServer := ironic.New(cfg, store, httpServer.ImageURL(), logger)

	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error { return httpServer.Run(gctx) })
	g.Go(func() error { return tftpServer.Run(gctx) })
	g.Go(func() error { return dhcpServer.Run(gctx) })
	g.Go(func() error { return ironicServer.Run(gctx) })

	if err := g.Wait(); err != nil && err != context.Canceled {
		logger.Error("server group exited", "err", err.Error())
		os.Exit(1)
	}
	logger.Info("autopxe shutdown complete")
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}
