package tftp

import (
	"context"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"path"
	"strings"
	"time"

	pintftp "github.com/pin/tftp/v3"

	"github.com/ZJUSCT/AutoPXE/internal/config"
)

type Server struct {
	cfg    *config.Config
	root   fs.FS
	logger *slog.Logger
	srv    *pintftp.Server
}

func New(cfg *config.Config, root fs.FS, logger *slog.Logger) *Server {
	return &Server{
		cfg:    cfg,
		root:   root,
		logger: logger.With("component", "tftp"),
	}
}

func (s *Server) Run(ctx context.Context) error {
	s.srv = pintftp.NewServer(s.read, nil)
	s.srv.SetTimeout(5 * time.Second)
	s.srv.EnableSinglePort()

	addr := fmt.Sprintf("%s:%d", s.cfg.Listen.IP, s.cfg.Listen.TFTPPort)

	errCh := make(chan error, 1)
	go func() { errCh <- s.srv.ListenAndServe(addr) }()

	s.logger.Info("tftp server listening", "addr", addr)

	select {
	case <-ctx.Done():
		s.srv.Shutdown()
		<-errCh
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func (s *Server) read(filename string, rf io.ReaderFrom) error {
	clean, err := sanitize(filename)
	if err != nil {
		s.logger.Warn("tftp reject", "filename", filename, "err", err.Error())
		return err
	}

	f, err := s.root.Open(clean)
	if err != nil {
		s.logger.Warn("tftp not found", "filename", clean, "err", err.Error())
		return fmt.Errorf("not found: %w", err)
	}
	defer f.Close()

	if info, err := fs.Stat(s.root, clean); err == nil {
		if setter, ok := rf.(pintftp.OutgoingTransfer); ok {
			setter.SetSize(info.Size())
		}
	}

	n, err := rf.ReadFrom(f)
	if err != nil {
		s.logger.Warn("tftp send failed", "filename", clean, "err", err.Error(), "bytes", n)
		return err
	}
	s.logger.Info("tftp sent", "filename", clean, "bytes", n)
	return nil
}

func sanitize(name string) (string, error) {
	if name == "" {
		return "", fmt.Errorf("empty filename")
	}
	if strings.ContainsAny(name, "\x00") {
		return "", fmt.Errorf("invalid filename")
	}
	clean := path.Clean("/" + name)
	clean = strings.TrimPrefix(clean, "/")
	if clean == "" || clean == "." || strings.HasPrefix(clean, "..") {
		return "", fmt.Errorf("path traversal rejected")
	}
	return clean, nil
}
