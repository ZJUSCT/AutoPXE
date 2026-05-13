package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"time"

	"golang.org/x/sync/errgroup"

	"github.com/ZJUSCT/AutoPXE/internal/assets"
	"github.com/ZJUSCT/AutoPXE/internal/config"
	"github.com/ZJUSCT/AutoPXE/internal/dhcp"
	"github.com/ZJUSCT/AutoPXE/internal/httpsrv"
	"github.com/ZJUSCT/AutoPXE/internal/ipadl"
	"github.com/ZJUSCT/AutoPXE/internal/ironic"
	"github.com/ZJUSCT/AutoPXE/internal/lease"
	"github.com/ZJUSCT/AutoPXE/internal/node"
	"github.com/ZJUSCT/AutoPXE/internal/state"
	"github.com/ZJUSCT/AutoPXE/internal/tftp"
)

func main() {
	cfgPath := flag.String("config", "/etc/autopxe/config.yaml", "path to config file")
	logLevel := flag.String("log-level", "info", "log level (debug, info, warn, error)")
	forget := flag.String("forget", "", "remove a MAC from the deployed-state file then exit (no servers are started)")
	forgetAll := flag.Bool("forget-all", false, "remove all entries from the deployed-state file then exit")
	listDeployed := flag.Bool("list-deployed", false, "print the deployed-state file contents then exit")
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
		"state_file", cfg.StateFile,
	)

	tracker, err := state.New(cfg.StateFile)
	if err != nil {
		logger.Error("load state file", "path", cfg.StateFile, "err", err.Error())
		os.Exit(1)
	}

	if handled := runMaintenance(tracker, *forget, *forgetAll, *listDeployed, logger); handled {
		return
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	if err := ipadl.EnsureArtifacts(ctx, cfg, logger); err != nil {
		logger.Error("ensure ipa artifacts", "err", err.Error())
		os.Exit(1)
	}

	imageHash, err := hashFileSHA256(ctx, cfg.Deploy.Image, logger)
	if err != nil {
		logger.Error("hash deploy image", "path", cfg.Deploy.Image, "err", err.Error())
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
	dhcpServer := dhcp.New(cfg, leaser, tracker, logger)
	ironicServer := ironic.New(cfg, store, tracker, httpServer.ImageURL(), imageHash, logger)

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

// hashFileSHA256 streams a file through SHA-256 and returns the lowercase hex
// digest. We compute this once at startup so every /v1/lookup and the
// standby.prepare_image dispatch can include os_hash_algo + os_hash_value
// (which IPA strictly requires when no legacy `checksum` is provided).
// For a 1–2 GB qcow2 this takes a few seconds; for very large images it can
// take a minute, but doing it eagerly means deploys can never race ahead of
// the hash and any I/O error surfaces before any DHCP / TFTP traffic is
// handed out.
func hashFileSHA256(ctx context.Context, path string, logger *slog.Logger) (string, error) {
	logger.Info("hashing deploy image", "path", path)
	start := time.Now()

	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	buf := make([]byte, 1<<20) // 1 MiB
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		default:
		}
		n, err := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
	}

	sum := hex.EncodeToString(h.Sum(nil))
	logger.Info("deploy image hashed",
		"path", path,
		"sha256", sum,
		"elapsed", time.Since(start).String(),
	)
	return sum, nil
}

// runMaintenance handles the one-shot CLI maintenance flags. Returns true when
// it serviced a flag and main should exit without starting servers.
func runMaintenance(tracker *state.Tracker, forget string, forgetAll, listDeployed bool, logger *slog.Logger) bool {
	switch {
	case listDeployed:
		records := tracker.Snapshot()
		if len(records) == 0 {
			fmt.Println("(no deployed records)")
			return true
		}
		for mac, rec := range records {
			fmt.Printf("%s  uuid=%s  at=%s  image_sha256=%s\n",
				mac, rec.UUID, rec.DeployedAt.Format(time.RFC3339), rec.ImageHash)
		}
		return true
	case forgetAll:
		if err := tracker.ForgetAll(); err != nil {
			logger.Error("forget-all", "err", err.Error())
			os.Exit(1)
		}
		logger.Info("forget-all: cleared all deployed records")
		return true
	case forget != "":
		removed, err := tracker.Forget(forget)
		if err != nil {
			logger.Error("forget", "mac", forget, "err", err.Error())
			os.Exit(1)
		}
		if removed {
			logger.Info("forget: removed deployed record", "mac", forget)
		} else {
			logger.Info("forget: mac not present", "mac", forget)
		}
		return true
	}
	return false
}

func newLogger(level string) *slog.Logger {
	var lv slog.Level
	if err := lv.UnmarshalText([]byte(level)); err != nil {
		lv = slog.LevelInfo
	}
	return slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: lv}))
}
