package configdrive

import (
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
)

// Generate builds a cloud-init Config Drive v2 ISO9660 image, gzips it, and
// returns the base64-encoded string that IPA's standby.prepare_image expects.
//
// IPA decode path: base64 → gzip → raw bytes. The raw bytes are dd'd to a
// partition and must be a mountable filesystem image (ISO9660 or VFAT).
// _does_config_drive_work mounts the partition; _try_build_fat32_config_drive
// mounts the raw bytes via loopback. A raw tarball fails both checks.
func Generate(nodeUUID, hostname, userData string, extraMeta map[string]string, networkData string) (string, error) {
	meta := map[string]string{
		"uuid": nodeUUID,
	}
	if hostname != "" {
		meta["hostname"] = hostname
	}
	for k, v := range extraMeta {
		meta[k] = v
	}
	metaJSON, err := json.Marshal(meta)
	if err != nil {
		return "", fmt.Errorf("marshal meta_data: %w", err)
	}

	dir, err := os.MkdirTemp("", "autopxe-cd-")
	if err != nil {
		return "", fmt.Errorf("temp dir: %w", err)
	}
	defer os.RemoveAll(dir)

	// Both "2012-08-10" and "latest" versions, matching the reference
	// implementation in openstacksdk/openstack/baremetal/configdrive.py.
	for _, version := range []string{"2012-08-10", "latest"} {
		subdir := filepath.Join(dir, "openstack", version)
		if err := os.MkdirAll(subdir, 0755); err != nil {
			return "", fmt.Errorf("mkdir %s: %w", version, err)
		}

		if err := os.WriteFile(filepath.Join(subdir, "meta_data.json"), metaJSON, 0644); err != nil {
			return "", fmt.Errorf("write meta_data: %w", err)
		}
		if userData != "" {
			if err := os.WriteFile(filepath.Join(subdir, "user_data"), []byte(userData), 0644); err != nil {
				return "", fmt.Errorf("write user_data: %w", err)
			}
		}
		if networkData != "" {
			if err := os.WriteFile(filepath.Join(subdir, "network_data.json"), []byte(networkData), 0644); err != nil {
				return "", fmt.Errorf("write network_data: %w", err)
			}
		}
	}

	isoPath := filepath.Join(dir, "configdrive.iso")
	if err := buildISO(isoPath, dir); err != nil {
		return "", fmt.Errorf("build ISO: %w", err)
	}

	isoBytes, err := os.ReadFile(isoPath)
	if err != nil {
		return "", fmt.Errorf("read iso: %w", err)
	}

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	if _, err := gw.Write(isoBytes); err != nil {
		gw.Close()
		return "", fmt.Errorf("gzip: %w", err)
	}
	if err := gw.Close(); err != nil {
		return "", fmt.Errorf("gzip close: %w", err)
	}

	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}

// buildISO tries genisoimage, mkisofs, xorrisofs in order, matching the
// reference implementation's fallback logic.
func buildISO(output, contentDir string) error {
	tools := []string{"genisoimage", "mkisofs", "xorrisofs"}
	// genisoimage, mkisofs and xorrisofs understand the same parameters used here.
	args := []string{
		"-o", output,
		"-ldots",
		"-allow-lowercase",
		"-allow-multidot",
		"-l",
		"-publisher", "autopxe",
		"-quiet",
		"-J",
		"-r",
		"-V", "config-2",
		contentDir,
	}

	var lastErr error
	for _, tool := range tools {
		if _, err := exec.LookPath(tool); err != nil {
			lastErr = fmt.Errorf("%s not found", tool)
			continue
		}
		cmd := exec.Command(tool, args...)
		if out, err := cmd.CombinedOutput(); err != nil {
			return fmt.Errorf("%s: %w: %s", tool, err, string(out))
		}
		return nil
	}
	return fmt.Errorf("no ISO tool available (tried %v): %w", tools, lastErr)
}
