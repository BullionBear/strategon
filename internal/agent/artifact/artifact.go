// Package artifact manages the on-disk, immutable release layout and atomic
// version switching described in ARCHITECTURE.md §8.1:
//
//	<base>/<strategy>/releases/<version>/bin           # the strategy binary
//	<base>/<strategy>/releases/<version>/config[.ext]  # optional config (ext from URI)
//	<base>/<strategy>/current -> releases/<version>    # atomic switch point
//	<base>/<strategy>/shared/                          # cross-version data
//
// Rollback is O(1): re-point the `current` symlink at an already-present
// release, no re-download. Fetching is pluggable; the v1 Fetcher is a
// local-filesystem copy so deploy/rollback is testable without S3/MinIO.
package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

// Fetcher retrieves an artifact's bytes into a destination file.
type Fetcher interface {
	// Fetch writes the artifact identified by ref to dest.
	Fetch(ctx context.Context, ref *pb.ArtifactRef, dest string) error
}

// Manager owns the release layout for all strategies under Base.
type Manager struct {
	Base    string
	Fetcher Fetcher
}

// NewManager returns a Manager rooted at base using fetcher.
func NewManager(base string, fetcher Fetcher) *Manager {
	return &Manager{Base: base, Fetcher: fetcher}
}

// StrategyDir is <base>/<strategy>.
func (m *Manager) StrategyDir(strategy string) string { return filepath.Join(m.Base, strategy) }

// ReleaseDir is <base>/<strategy>/releases/<version>.
func (m *Manager) ReleaseDir(strategy, version string) string {
	return filepath.Join(m.StrategyDir(strategy), "releases", version)
}

// CurrentLink is <base>/<strategy>/current.
func (m *Manager) CurrentLink(strategy string) string {
	return filepath.Join(m.StrategyDir(strategy), "current")
}

// BinaryPath is the path of the binary in a specific release.
func (m *Manager) BinaryPath(strategy, version string) string {
	return filepath.Join(m.ReleaseDir(strategy, version), "bin")
}

// CurrentBinaryPath resolves the binary via the current symlink.
func (m *Manager) CurrentBinaryPath(strategy string) string {
	return filepath.Join(m.CurrentLink(strategy), "bin")
}

// ConfigFileName derives the on-disk config basename from an artifact URI,
// preserving the original extension (e.g. config.yml). No extension → "config".
func ConfigFileName(ref *pb.ArtifactRef) string {
	if ref == nil {
		return "config"
	}
	ext := filepath.Ext(uriPath(ref.GetUri()))
	if ext == "" || ext == "." {
		return "config"
	}
	return "config" + ext
}

// ConfigPath is the config file path in a specific release.
func (m *Manager) ConfigPath(strategy, version string, ref *pb.ArtifactRef) string {
	return filepath.Join(m.ReleaseDir(strategy, version), ConfigFileName(ref))
}

// CurrentConfigPath resolves the config file via the current symlink.
func (m *Manager) CurrentConfigPath(strategy string, ref *pb.ArtifactRef) string {
	return filepath.Join(m.CurrentLink(strategy), ConfigFileName(ref))
}

// CurrentReleaseDir returns the absolute path of the release directory that
// current points at (symlink resolved).
func (m *Manager) CurrentReleaseDir(strategy string) (string, error) {
	link := m.CurrentLink(strategy)
	abs, err := filepath.Abs(link)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.EvalSymlinks(abs)
	if err != nil {
		return "", err
	}
	return resolved, nil
}

func uriPath(uri string) string {
	// Strip common schemes so filepath.Ext sees the real filename.
	for _, prefix := range []string{"file://", "http://", "https://", "s3://"} {
		if strings.HasPrefix(uri, prefix) {
			uri = strings.TrimPrefix(uri, prefix)
			break
		}
	}
	// Drop query/fragment if present (http/s3 style).
	if i := strings.IndexAny(uri, "?#"); i >= 0 {
		uri = uri[:i]
	}
	return uri
}

// HasRelease reports whether a release version is already present locally
// (enables O(1), no-download rollback).
func (m *Manager) HasRelease(strategy, version string) bool {
	_, err := os.Stat(m.BinaryPath(strategy, version))
	return err == nil
}

// Download fetches the artifact (and optional config) into its release dir. It
// is idempotent: an already-present, verified release is left untouched.
func (m *Manager) Download(ctx context.Context, strategy string, artifactRef, configRef *pb.ArtifactRef) error {
	dir := m.ReleaseDir(strategy, artifactRef.GetVersion())
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir release: %w", err)
	}
	bin := m.BinaryPath(strategy, artifactRef.GetVersion())
	if err := m.Fetcher.Fetch(ctx, artifactRef, bin); err != nil {
		return fmt.Errorf("fetch binary: %w", err)
	}
	if err := os.Chmod(bin, 0o755); err != nil {
		return fmt.Errorf("chmod binary: %w", err)
	}
	if configRef != nil && configRef.GetDigest() != "" {
		cfg := m.ConfigPath(strategy, artifactRef.GetVersion(), configRef)
		if err := m.Fetcher.Fetch(ctx, configRef, cfg); err != nil {
			return fmt.Errorf("fetch config: %w", err)
		}
	}
	return nil
}

// Verify checks the SHA256 of the downloaded binary against ref.digest
// ("sha256:...").
func (m *Manager) Verify(strategy string, ref *pb.ArtifactRef) error {
	want := ref.GetDigest()
	if want == "" {
		return fmt.Errorf("verify: empty digest for %s", ref.GetVersion())
	}
	sum, err := fileSHA256(m.BinaryPath(strategy, ref.GetVersion()))
	if err != nil {
		return err
	}
	got := "sha256:" + sum
	if !strings.EqualFold(got, want) {
		return fmt.Errorf("verify %s: digest mismatch got %s want %s", ref.GetVersion(), got, want)
	}
	return nil
}

// SwitchTo atomically re-points current -> releases/<version> via a temp
// symlink + rename (rename within a directory is atomic).
func (m *Manager) SwitchTo(strategy, version string) error {
	target := filepath.Join("releases", version) // relative link
	link := m.CurrentLink(strategy)
	tmp := link + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("symlink tmp: %w", err)
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("atomic rename: %w", err)
	}
	return nil
}

// CurrentVersion reads the version the current symlink points at, or "" if
// unset.
func (m *Manager) CurrentVersion(strategy string) string {
	dst, err := os.Readlink(m.CurrentLink(strategy))
	if err != nil {
		return ""
	}
	return filepath.Base(dst)
}

func fileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}
