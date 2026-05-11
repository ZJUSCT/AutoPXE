package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"crypto/rand"
	"encoding/hex"
)

// ---- Config ----

type Config struct {
	Listen       string `json:"listen"`        // e.g. ":6385"
	ImageSource  string `json:"image_source"`  // URL to qcow2 image
	ImageType    string `json:"image_type"`    // "partition" or "whole_disk"
	DiskFormat   string `json:"disk_format"`   // "qcow2", "raw"
	RootGB       int    `json:"root_gb"`        // root partition size in GB
	RootMB       int    `json:"root_mb"`        // root partition size in MB (overrides root_gb)
}

var config Config

// ---- Node State ----

type NodeState string

const (
	StateNew        NodeState = "new"
	StateDeploying  NodeState = "deploying"
	StateDone       NodeState = "done"
)

type Node struct {
	UUID           string
	MAC            string
	CallbackURL    string
	State          NodeState
	CommandID      string // last command ID sent to IPA
	LastHeartbeat  time.Time
	mu             sync.Mutex
}

type NodeStore struct {
	mu    sync.RWMutex
	nodes map[string]*Node // UUID -> Node
	macIndex map[string]string // MAC -> UUID
}

var store = &NodeStore{
	nodes:    make(map[string]*Node),
	macIndex: make(map[string]string),
}

func (s *NodeStore) GetByMAC(mac string) *Node {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if uuid, ok := s.macIndex[mac]; ok {
		return s.nodes[uuid]
	}
	return nil
}

func (s *NodeStore) GetByUUID(uuid string) *Node {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.nodes[uuid]
}

func (s *NodeStore) Create(mac string) *Node {
	s.mu.Lock()
	defer s.mu.Unlock()
	uuid := newUUID()
	node := &Node{
		UUID:  uuid,
		MAC:   mac,
		State: StateNew,
	}
	s.nodes[uuid] = node
	s.macIndex[mac] = uuid
	return node
}

func newUUID() string {
	b := make([]byte, 16)
	rand.Read(b)
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%s-%s-%s-%s-%s",
		hex.EncodeToString(b[0:4]),
		hex.EncodeToString(b[4:6]),
		hex.EncodeToString(b[6:8]),
		hex.EncodeToString(b[8:10]),
		hex.EncodeToString(b[10:16]))
}

// ---- HTTP Handlers ----

func handleRoot(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, 200, map[string]interface{}{
		"name":        "fakonic",
		"description": "Minimal Ironic API for PXE cluster deployment",
		"default_version": map[string]interface{}{
			"id":      "v1",
			"version": "1.67",
		},
		"versions": []map[string]interface{}{
			{"id": "v1", "version": "1.67"},
		},
	})
}

func handleLookup(w http.ResponseWriter, r *http.Request) {
	addresses := r.URL.Query().Get("addresses")
	if addresses == "" {
		writeJSON(w, 400, map[string]string{"error": "addresses required"})
		return
	}

	// Use first MAC address
	mac := strings.Split(addresses, ",")[0]
	mac = strings.ToLower(strings.TrimSpace(mac))

	node := store.GetByMAC(mac)
	if node == nil {
		node = store.Create(mac)
		log.Printf("[lookup] new node: mac=%s uuid=%s", mac, node.UUID)
	} else {
		log.Printf("[lookup] existing node: mac=%s uuid=%s state=%s", mac, node.UUID, node.State)
	}

	rootMB := config.RootMB
	if rootMB == 0 && config.RootGB > 0 {
		rootMB = config.RootGB * 1024
	}
	if rootMB == 0 {
		rootMB = 51200 // default 50GB
	}

	imageID := fmt.Sprintf("img-%s", node.UUID[:8])

	writeJSON(w, 200, map[string]interface{}{
		"node": map[string]interface{}{
			"uuid":       node.UUID,
			"properties": map[string]interface{}{},
			"driver_internal_info": map[string]interface{}{},
			"instance_info": map[string]interface{}{
				"id":              imageID,
				"urls":            []string{config.ImageSource},
				"image_type":      config.ImageType,
				"disk_format":     config.DiskFormat,
				"root_mb":         rootMB,
				"node_uuid":       node.UUID,
				"preserve_ephemeral": false,
			"swap_mb": 0,
			"ephemeral_mb": 0,
			"ephemeral_format": "ext4",
			},
		},
		"config": map[string]interface{}{
			"heartbeat_timeout":    300,
			"agent_token":          "fakonic-test-token-32-chars-minx",
			"agent_token_required": true,
				"disable_deep_image_inspection": true,
		},
	})
}

func handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	// Extract UUID from path: /v1/heartbeat/<uuid>
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/heartbeat/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeJSON(w, 400, map[string]string{"error": "missing UUID"})
		return
	}
	uuid := parts[0]

	node := store.GetByUUID(uuid)
	if node == nil {
		log.Printf("[heartbeat] unknown UUID: %s", uuid)
		writeJSON(w, 404, map[string]string{"error": "node not found"})
		return
	}

	var body struct {
		CallbackURL string `json:"callback_url"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeJSON(w, 400, map[string]string{"error": "invalid JSON"})
		return
	}

	node.mu.Lock()
	node.CallbackURL = strings.TrimRight(body.CallbackURL, "/")
	node.LastHeartbeat = time.Now()
	state := node.State
	node.mu.Unlock()

	log.Printf("[heartbeat] uuid=%s state=%s callback=%s", uuid, state, node.CallbackURL)

	switch state {
	case StateNew:
		go dispatchDeploy(node)
	case StateDeploying:
		go pollDeployStatus(node)
	}

	w.WriteHeader(202)
}

// ---- Deploy Dispatch ----

func dispatchDeploy(node *Node) {
	node.mu.Lock()
	node.State = StateDeploying
	node.mu.Unlock()

	// Build image_info from the stored config
	rootMB := config.RootMB
	if rootMB == 0 && config.RootGB > 0 {
		rootMB = config.RootGB * 1024
	}
	if rootMB == 0 {
		rootMB = 51200
	}
	imageID := fmt.Sprintf("img-%s", node.UUID[:8])

	imageInfo := map[string]interface{}{
		"id":              imageID,
		"urls":            []string{config.ImageSource},
		"image_type":      config.ImageType,
		"disk_format":     config.DiskFormat,
		"root_mb":         rootMB,
		"node_uuid":       node.UUID,
		"preserve_ephemeral": false,
			"swap_mb": 0,
			"ephemeral_mb": 0,
			"ephemeral_format": "ext4",
			"os_hash_algo":   "sha256",
			"os_hash_value":  "640fb285344587a0544fc52fb56164761eeae09920741a3c61bdcc8854a5c377",
		}

		result, err := callIPAAgent(node.CallbackURL, "standby.prepare_image", map[string]interface{}{
		"image_info": imageInfo,
	})
	if err != nil {
		log.Printf("[deploy] node=%s prepare_image failed: %v", node.UUID, err)
		return
	}
	log.Printf("[deploy] node=%s prepare_image result: %v", node.UUID, result)

	cmdID, _ := result["id"].(string)
	node.mu.Lock()
	node.CommandID = cmdID
	node.mu.Unlock()
}

func pollDeployStatus(node *Node) {
	node.mu.Lock()
	cmdID := node.CommandID
	node.mu.Unlock()

	if cmdID == "" {
		return
	}

	status, err := getIPACommandStatus(node.CallbackURL, cmdID)
		log.Printf("[deploy] node=%s command=%s raw=%v", node.UUID, cmdID, status)
	if err != nil {
		log.Printf("[deploy] node=%s poll failed: %v", node.UUID, err)
		return
	}

	cmdStatus, _ := status["command_status"].(string)
	log.Printf("[deploy] node=%s command=%s status=%s", node.UUID, cmdID, cmdStatus)

	switch cmdStatus {
	case "SUCCEEDED":
		// Deployment complete — reboot into deployed OS
		log.Printf("[deploy] node=%s deploy succeeded, calling run_image", node.UUID)
		callIPAAgent(node.CallbackURL, "standby.run_image", nil)
		node.mu.Lock()
		node.State = StateDone
		node.mu.Unlock()
	case "FAILED":
		log.Printf("[deploy] node=%s deploy FAILED: %v", node.UUID, status["command_error"])
		node.mu.Lock()
		node.State = StateNew // reset to retry
		node.CommandID = ""
		node.mu.Unlock()
	}
}

// ---- IPA Agent HTTP Client ----

func callIPAAgent(agentURL, commandName string, params map[string]interface{}) (map[string]interface{}, error) {
	url := fmt.Sprintf("%s/v1/commands/?agent_token=fakonic-test-token-32-chars-minx", agentURL)

	body := map[string]interface{}{
		"name":   commandName,
		"params": params,
	}
	bodyJSON, _ := json.Marshal(body)

	req, err := http.NewRequest("POST", url, bytes.NewReader(bodyJSON))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return result, nil
}

func getIPACommandStatus(agentURL, cmdID string) (map[string]interface{}, error) {
	url := fmt.Sprintf("%s/v1/commands/%s?agent_token=fakonic-test-token-32-chars-minx", agentURL, cmdID)

	client := &http.Client{Timeout: 10 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()

	respBody, _ := io.ReadAll(resp.Body)

	var result map[string]interface{}
	if err := json.Unmarshal(respBody, &result); err != nil {
		return nil, fmt.Errorf("parse response: %w", err)
	}
	return result, nil
}

// ---- Helpers ----

func writeJSON(w http.ResponseWriter, status int, v interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func loadConfig(path string) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, &config)
}

func loggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Printf("[http] %s %s %s", r.Method, r.URL.Path, r.RemoteAddr)
		next.ServeHTTP(w, r)
	})
}

func main() {
	configPath := os.Getenv("FAKONIC_CONFIG")
	if configPath == "" {
		configPath = "/etc/fakonic/config.json"
	}
	if err := loadConfig(configPath); err != nil {
		log.Fatalf("Failed to load config: %v", err)
	}

	if config.Listen == "" {
		config.Listen = ":6385"
	}
	if config.ImageType == "" {
		config.ImageType = "partition"
	}
	if config.DiskFormat == "" {
		config.DiskFormat = "qcow2"
	}

	log.Printf("fakonic starting on %s", config.Listen)
	log.Printf("  image_source: %s", config.ImageSource)
	log.Printf("  image_type:   %s", config.ImageType)
	log.Printf("  disk_format:  %s", config.DiskFormat)
	if config.RootMB > 0 {
		log.Printf("  root_mb:      %d", config.RootMB)
	} else {
		log.Printf("  root_gb:      %d", config.RootGB)
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		switch {
		case path == "/" || path == "/v1" || path == "/v1/":
			handleRoot(w, r)
		case path == "/v1/lookup":
			handleLookup(w, r)
		case strings.HasPrefix(path, "/v1/heartbeat/"):
			handleHeartbeat(w, r)
		default:
			writeJSON(w, 404, map[string]string{"error": "not found"})
		}
	})

	server := &http.Server{
		Addr:         config.Listen,
		Handler:      loggingMiddleware(mux),
		ReadTimeout:  10 * time.Second,
		WriteTimeout: 10 * time.Second,
	}

	if err := server.ListenAndServe(); err != nil {
		log.Fatalf("Server failed: %v", err)
	}
}
