package httpsrv

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"path/filepath"
	"strings"
	"text/template"
	"time"

	"github.com/ZJUSCT/AutoPXE/internal/assets"
	"github.com/ZJUSCT/AutoPXE/internal/config"
)

type Server struct {
	cfg          *config.Config
	logger       *slog.Logger
	bootTemplate *template.Template
	imageName    string
	srv          *http.Server
}

func New(cfg *config.Config, logger *slog.Logger) *Server {
	return &Server{
		cfg:          cfg,
		logger:       logger.With("component", "http"),
		bootTemplate: assets.BootIPXETemplate(),
		imageName:    filepath.Base(cfg.Deploy.Image),
	}
}

// ImageURL is the canonical URL the ironic API returns to IPA for the deploy image.
func (s *Server) ImageURL() string {
	return fmt.Sprintf("http://%s:%d/images/%s",
		s.cfg.Listen.IP, s.cfg.Listen.HTTPPort, s.imageName)
}

func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/boot.ipxe", s.handleBootIPXE)
	mux.HandleFunc("/ipa.kernel", s.handleStatic(s.cfg.IPA.Kernel))
	mux.HandleFunc("/ipa.initramfs", s.handleStatic(s.cfg.IPA.Initramfs))
	mux.HandleFunc("/images/", s.handleImage)
	mux.HandleFunc("/", s.handleIndex)

	addr := fmt.Sprintf("%s:%d", s.cfg.Listen.IP, s.cfg.Listen.HTTPPort)
	s.srv = &http.Server{
		Addr:    addr,
		Handler: s.logging(mux),
		// No WriteTimeout — qcow2 transfers can take many minutes.
		ReadHeaderTimeout: 15 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}

	errCh := make(chan error, 1)
	go func() {
		err := s.srv.ListenAndServe()
		if err == http.ErrServerClosed {
			err = nil
		}
		errCh <- err
	}()

	s.logger.Info("http server listening", "addr", addr)

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = s.srv.Shutdown(shutdownCtx)
		<-errCh
		return ctx.Err()
	case err := <-errCh:
		return err
	}
}

func (s *Server) handleBootIPXE(w http.ResponseWriter, r *http.Request) {
	mac := r.URL.Query().Get("mac")
	data := struct {
		KernelURL, InitramfsURL, IronicURL, MAC string
	}{
		KernelURL:    fmt.Sprintf("http://%s:%d/ipa.kernel", s.cfg.Listen.IP, s.cfg.Listen.HTTPPort),
		InitramfsURL: fmt.Sprintf("http://%s:%d/ipa.initramfs", s.cfg.Listen.IP, s.cfg.Listen.HTTPPort),
		IronicURL:    fmt.Sprintf("http://%s:%d", s.cfg.Listen.IP, s.cfg.Listen.IronicPort),
		MAC:          mac,
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	if err := s.bootTemplate.Execute(w, data); err != nil {
		s.logger.Error("boot.ipxe render", "err", err.Error())
	}
}

func (s *Server) handleStatic(path string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		http.ServeFile(w, r, path)
	}
}

func (s *Server) handleImage(w http.ResponseWriter, r *http.Request) {
	requested := strings.TrimPrefix(r.URL.Path, "/images/")
	if requested == "" || strings.Contains(requested, "..") || strings.ContainsAny(requested, "/\x00") {
		http.NotFound(w, r)
		return
	}
	if requested != s.imageName {
		http.NotFound(w, r)
		return
	}
	http.ServeFile(w, r, s.cfg.Deploy.Image)
}

func (s *Server) handleIndex(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	fmt.Fprintf(w, "autopxe\n  /boot.ipxe\n  /ipa.kernel\n  /ipa.initramfs\n  /images/%s\n", s.imageName)
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.logger.Info("http request", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
		next.ServeHTTP(w, r)
	})
}
