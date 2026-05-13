package ironic

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"autopxe/internal/config"
	"autopxe/internal/node"
)

type Server struct {
	cfg       *config.Config
	store     *node.Store
	imageURL  string
	imageHash string // sha256 hex of cfg.Deploy.Image
	logger    *slog.Logger
	srv       *http.Server
}

func New(cfg *config.Config, store *node.Store, imageURL, imageHashSHA256 string, logger *slog.Logger) *Server {
	return &Server{
		cfg:       cfg,
		store:     store,
		imageURL:  imageURL,
		imageHash: imageHashSHA256,
		logger:    logger.With("component", "ironic"),
	}
}

func (s *Server) Run(ctx context.Context) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/", s.handleRoot)
	mux.HandleFunc("/v1", s.handleRoot)
	mux.HandleFunc("/v1/", s.handleRoot)
	mux.HandleFunc("/v1/lookup", s.handleLookup)
	mux.HandleFunc("/v1/heartbeat/", s.handleHeartbeat)

	addr := fmt.Sprintf("%s:%d", s.cfg.Listen.IP, s.cfg.Listen.IronicPort)
	s.srv = &http.Server{
		Addr:              addr,
		Handler:           s.logging(mux),
		ReadHeaderTimeout: 10 * time.Second,
	}

	errCh := make(chan error, 1)
	go func() {
		err := s.srv.ListenAndServe()
		if err == http.ErrServerClosed {
			err = nil
		}
		errCh <- err
	}()

	s.logger.Info("ironic server listening", "addr", addr)

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

func (s *Server) handleRoot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"name":        "autopxe",
		"description": "Ironic-compatible API for autopxe cluster deployment",
		"default_version": map[string]any{
			"id":      "v1",
			"version": "1.67",
		},
		"versions": []map[string]any{
			{"id": "v1", "version": "1.67"},
		},
	})
}

func (s *Server) handleLookup(w http.ResponseWriter, r *http.Request) {
	addresses := r.URL.Query().Get("addresses")
	if addresses == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "addresses required"})
		return
	}

	macs := splitMACs(addresses)
	if len(macs) == 0 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "no valid mac addresses"})
		return
	}

	n, created := s.store.GetOrCreate(macs)
	if n == nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "could not register node"})
		return
	}

	if created {
		s.logger.Info("lookup created node", "uuid", n.UUID, "macs", n.MACs)
	} else {
		s.logger.Info("lookup matched node", "uuid", n.UUID, "state", n.State())
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"node": map[string]any{
			"uuid":                 n.UUID,
			"properties":           map[string]any{},
			"driver_internal_info": map[string]any{},
			"instance_info":        s.instanceInfo(n.UUID),
		},
		"config": map[string]any{
			"heartbeat_timeout":             s.cfg.Ironic.HeartbeatTimeout,
			"agent_token":                   n.AgentToken,
			"agent_token_required":          true,
			"disable_deep_image_inspection": true,
		},
	})
}

func (s *Server) instanceInfo(nodeUUID string) map[string]any {
	info := map[string]any{
		"id":                 fmt.Sprintf("img-%s", nodeUUID[:8]),
		"urls":               []string{s.imageURL},
		"image_type":         s.cfg.Deploy.ImageType,
		"disk_format":        s.cfg.Deploy.DiskFormat,
		"root_mb":            s.cfg.Deploy.RootMB,
		"node_uuid":          nodeUUID,
		"preserve_ephemeral": false,
		"swap_mb":            0,
		"ephemeral_mb":       0,
		"ephemeral_format":   "ext4",
	}
	// IPA's standby.prepare_image strictly requires either a legacy
	// `checksum` field or the `os_hash_algo` + `os_hash_value` pair.
	if s.imageHash != "" {
		info["os_hash_algo"] = "sha256"
		info["os_hash_value"] = s.imageHash
	}
	return info
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	uuid := strings.TrimPrefix(r.URL.Path, "/v1/heartbeat/")
	uuid = strings.Trim(uuid, "/")
	if uuid == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing uuid"})
		return
	}

	n := s.store.GetByUUID(uuid)
	if n == nil {
		s.logger.Warn("heartbeat for unknown uuid", "uuid", uuid)
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "node not found"})
		return
	}

	var body struct {
		CallbackURL string `json:"callback_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid json"})
		return
	}
	body.CallbackURL = strings.TrimRight(body.CallbackURL, "/")

	startDriver := n.RecordHeartbeat(body.CallbackURL)

	s.logger.Info("heartbeat",
		"uuid", uuid,
		"state", n.State(),
		"callback", body.CallbackURL,
		"start_driver", startDriver,
	)

	if startDriver {
		go s.driveDeploy(n)
	}

	w.WriteHeader(http.StatusAccepted)
}

func splitMACs(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		out = append(out, p)
	}
	return out
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) logging(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.logger.Debug("request", "method", r.Method, "path", r.URL.Path, "remote", r.RemoteAddr)
		next.ServeHTTP(w, r)
	})
}
