package ipadl

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ZJUSCT/AutoPXE/internal/config"
)

// EnsureArtifacts makes sure the IPA kernel and initramfs exist at the paths
// configured in cfg.IPA. If both are already present, this is a no-op. If
// either is missing, the IPA tarball is downloaded synchronously, extracted,
// and the kernel/initramfs files are placed at their target paths.
//
// Synchronous on purpose: DHCP must not OFFER a boot before the artifacts
// exist on disk, otherwise the first node to PXE will fetch a 404.
func EnsureArtifacts(ctx context.Context, cfg *config.Config, logger *slog.Logger) error {
	logger = logger.With("component", "ipadl")

	if fileExists(cfg.IPA.Kernel) && fileExists(cfg.IPA.Initramfs) {
		logger.Info("artifacts present", "kernel", cfg.IPA.Kernel, "initramfs", cfg.IPA.Initramfs)
		return nil
	}

	dl := cfg.IPA.Download
	tarballName := fmt.Sprintf("ipa-%s-%s.tar.gz", dl.Flavor, dl.Branch)
	tarballURL := strings.TrimRight(dl.BaseURI, "/") + "/" + tarballName
	prefix := strings.TrimSuffix(tarballName, ".tar.gz") // ipa-centos9-master

	if err := os.MkdirAll(dl.CacheDir, 0o755); err != nil {
		return fmt.Errorf("mkdir cache: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.IPA.Kernel), 0o755); err != nil {
		return fmt.Errorf("mkdir kernel target: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(cfg.IPA.Initramfs), 0o755); err != nil {
		return fmt.Errorf("mkdir initramfs target: %w", err)
	}

	tarballPath := filepath.Join(dl.CacheDir, tarballName)
	if !fileExists(tarballPath) {
		logger.Info("downloading IPA tarball", "url", tarballURL, "dest", tarballPath)
		if err := downloadFile(ctx, tarballURL, tarballPath); err != nil {
			return fmt.Errorf("download tarball: %w", err)
		}
	} else {
		logger.Info("tarball already cached", "path", tarballPath)
	}

	logger.Info("extracting IPA tarball", "path", tarballPath)
	if err := extractKernelInitramfs(tarballPath, prefix, cfg.IPA.Kernel, cfg.IPA.Initramfs); err != nil {
		return fmt.Errorf("extract: %w", err)
	}

	logger.Info("artifacts ready", "kernel", cfg.IPA.Kernel, "initramfs", cfg.IPA.Initramfs)
	return nil
}

func fileExists(p string) bool {
	st, err := os.Stat(p)
	return err == nil && !st.IsDir()
}

func downloadFile(ctx context.Context, url, dest string) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return err
	}
	client := &http.Client{Timeout: 30 * time.Minute}
	resp, err := client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, resp.Body); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}

func extractKernelInitramfs(tarballPath, prefix, kernelDest, initramfsDest string) error {
	f, err := os.Open(tarballPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	tr := tar.NewReader(gz)

	wantKernel := prefix + ".kernel"
	wantInitramfs := prefix + ".initramfs"
	gotKernel, gotInitramfs := false, false

	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar read: %w", err)
		}
		if hdr.Typeflag != tar.TypeReg {
			continue
		}
		base := filepath.Base(hdr.Name)
		switch base {
		case wantKernel:
			if err := copyToFile(tr, kernelDest); err != nil {
				return err
			}
			gotKernel = true
		case wantInitramfs:
			if err := copyToFile(tr, initramfsDest); err != nil {
				return err
			}
			gotInitramfs = true
		}
		if gotKernel && gotInitramfs {
			break
		}
	}
	if !gotKernel {
		return fmt.Errorf("kernel %q not found in tarball", wantKernel)
	}
	if !gotInitramfs {
		return fmt.Errorf("initramfs %q not found in tarball", wantInitramfs)
	}
	return nil
}

func copyToFile(r io.Reader, dest string) error {
	tmp := dest + ".part"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	if _, err := io.Copy(f, r); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dest)
}
