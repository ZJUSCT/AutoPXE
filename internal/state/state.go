// Package state persists the set of MAC addresses that have already been
// successfully deployed. Once a MAC is recorded here, autopxe stops handing
// out boot files for it via DHCP — the firmware's PXE attempt fails and the
// machine falls through to the next boot device (local disk).
//
// The store is a single JSON file written atomically (.tmp + rename). It is
// safe for concurrent use across DHCP / ironic goroutines.
package state

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

const currentVersion = 1

type Record struct {
	UUID       string    `json:"uuid"`
	DeployedAt time.Time `json:"deployed_at"`
	ImageHash  string    `json:"image_hash,omitempty"`
}

type fileFormat struct {
	Version  int               `json:"version"`
	Deployed map[string]Record `json:"deployed"`
}

type Tracker struct {
	path string
	mu   sync.RWMutex
	data fileFormat
}

func New(path string) (*Tracker, error) {
	t := &Tracker{
		path: path,
		data: fileFormat{
			Version:  currentVersion,
			Deployed: map[string]Record{},
		},
	}
	if err := t.load(); err != nil {
		return nil, err
	}
	return t, nil
}

func (t *Tracker) load() error {
	raw, err := os.ReadFile(t.path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return fmt.Errorf("read state: %w", err)
	}
	var f fileFormat
	if err := json.Unmarshal(raw, &f); err != nil {
		return fmt.Errorf("parse state %s: %w", t.path, err)
	}
	if f.Deployed == nil {
		f.Deployed = map[string]Record{}
	}
	t.data = f
	return nil
}

func (t *Tracker) save() error {
	if err := os.MkdirAll(filepath.Dir(t.path), 0o755); err != nil {
		return fmt.Errorf("mkdir state dir: %w", err)
	}
	t.data.Version = currentVersion
	raw, err := json.MarshalIndent(t.data, "", "  ")
	if err != nil {
		return err
	}
	tmp := t.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, t.path)
}

func canon(mac string) string {
	if hw, err := net.ParseMAC(mac); err == nil {
		return strings.ToLower(hw.String())
	}
	return strings.ToLower(strings.TrimSpace(mac))
}

// IsDeployed reports whether the given MAC has a deployment record.
func (t *Tracker) IsDeployed(mac string) bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	_, ok := t.data.Deployed[canon(mac)]
	return ok
}

// MarkDeployed records every MAC in the slice with the same Record. All MACs
// belonging to a multi-NIC node are persisted together so a future PXE
// attempt over any of them is gated.
func (t *Tracker) MarkDeployed(macs []string, uuid, imageHash string) error {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec := Record{
		UUID:       uuid,
		DeployedAt: time.Now().UTC(),
		ImageHash:  imageHash,
	}
	for _, m := range macs {
		c := canon(m)
		if c == "" {
			continue
		}
		t.data.Deployed[c] = rec
	}
	return t.save()
}

// Forget removes a single MAC from the deployed set. Returns true if the MAC
// was present.
func (t *Tracker) Forget(mac string) (bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	c := canon(mac)
	if _, ok := t.data.Deployed[c]; !ok {
		return false, nil
	}
	delete(t.data.Deployed, c)
	return true, t.save()
}

// ForgetAll clears all deployment records.
func (t *Tracker) ForgetAll() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.data.Deployed = map[string]Record{}
	return t.save()
}

// Snapshot returns a copy of the deployed map for inspection / display.
func (t *Tracker) Snapshot() map[string]Record {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make(map[string]Record, len(t.data.Deployed))
	for k, v := range t.data.Deployed {
		out[k] = v
	}
	return out
}
