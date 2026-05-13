package configdrive

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"fmt"
)

// Generate builds a cloud-init Config Drive v2 tarball, gzips it, and returns
// the base64-encoded string that IPA's standby.prepare_image expects.
//
// The tarball always contains openstack/latest/meta_data.json. If userData is
// non-empty, openstack/latest/user_data is included. If networkData is
// non-empty, openstack/latest/network_data.json is included.
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

	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)

	addFile := func(name string, data []byte) error {
		hdr := &tar.Header{
			Name: name,
			Mode: 0644,
			Size: int64(len(data)),
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		_, err := tw.Write(data)
		return err
	}

	if err := addFile("openstack/latest/meta_data.json", metaJSON); err != nil {
		return "", fmt.Errorf("add meta_data: %w", err)
	}
	if userData != "" {
		if err := addFile("openstack/latest/user_data", []byte(userData)); err != nil {
			return "", fmt.Errorf("add user_data: %w", err)
		}
	}
	if networkData != "" {
		if err := addFile("openstack/latest/network_data.json", []byte(networkData)); err != nil {
			return "", fmt.Errorf("add network_data: %w", err)
		}
	}

	if err := tw.Close(); err != nil {
		return "", fmt.Errorf("close tar: %w", err)
	}
	if err := gw.Close(); err != nil {
		return "", fmt.Errorf("close gzip: %w", err)
	}

	return base64.StdEncoding.EncodeToString(buf.Bytes()), nil
}
