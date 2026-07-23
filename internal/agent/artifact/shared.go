package artifact

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/sharedfile"
)

// SharedRoot is <base>/shared — the machine-level shared-file root.
func (m *Manager) SharedRoot() string { return filepath.Join(m.Base, "shared") }

// SharedStoreDir is <base>/shared/store.
func (m *Manager) SharedStoreDir() string { return filepath.Join(m.SharedRoot(), "store") }

// digestDirName strips an optional "sha256:" prefix for use as a store directory name.
func digestDirName(digest string) string {
	d := strings.TrimPrefix(strings.ToLower(digest), "sha256:")
	return d
}

// SharedStorePath is <base>/shared/store/<digest>/<name>.
func (m *Manager) SharedStorePath(digest, name string) string {
	return filepath.Join(m.SharedStoreDir(), digestDirName(digest), name)
}

// SharedLink is <base>/shared/<name> — the atomic switch point symlink.
func (m *Manager) SharedLink(name string) string {
	return filepath.Join(m.SharedRoot(), name)
}

// EnsureSharedFile fetches ref into the content-addressed store and verifies
// the digest. Does not switch the live symlink.
func (m *Manager) EnsureSharedFile(ctx context.Context, name string, ref *pb.ArtifactRef) error {
	if err := sharedfile.ValidateName(name); err != nil {
		return err
	}
	if ref == nil || ref.GetDigest() == "" {
		return fmt.Errorf("ensure shared %s: empty artifact digest", name)
	}
	dest := m.SharedStorePath(ref.GetDigest(), name)
	if err := os.MkdirAll(filepath.Dir(dest), 0o755); err != nil {
		return fmt.Errorf("mkdir shared store: %w", err)
	}
	// Skip re-fetch when a verified store entry already exists.
	if sum, err := fileSHA256(dest); err == nil {
		got := "sha256:" + sum
		if strings.EqualFold(got, ref.GetDigest()) {
			return nil
		}
	}
	if err := m.Fetcher.Fetch(ctx, ref, dest); err != nil {
		return fmt.Errorf("fetch shared %s: %w", name, err)
	}
	sum, err := fileSHA256(dest)
	if err != nil {
		return err
	}
	got := "sha256:" + sum
	if !strings.EqualFold(got, ref.GetDigest()) {
		_ = os.Remove(dest)
		return fmt.Errorf("verify shared %s: digest mismatch got %s want %s", name, got, ref.GetDigest())
	}
	return nil
}

// SwitchSharedTo atomically re-points <base>/shared/<name> at the store entry
// for digest via temp symlink + rename.
func (m *Manager) SwitchSharedTo(name, digest string) error {
	if err := sharedfile.ValidateName(name); err != nil {
		return err
	}
	storePath := m.SharedStorePath(digest, name)
	if _, err := os.Stat(storePath); err != nil {
		return fmt.Errorf("switch shared %s: store entry missing: %w", name, err)
	}
	if err := os.MkdirAll(m.SharedRoot(), 0o755); err != nil {
		return fmt.Errorf("mkdir shared root: %w", err)
	}
	// Relative target from <base>/shared/<name> → store/<digest>/<name>
	target := filepath.Join("store", digestDirName(digest), name)
	link := m.SharedLink(name)
	tmp := link + ".tmp"
	_ = os.Remove(tmp)
	if err := os.Symlink(target, tmp); err != nil {
		return fmt.Errorf("symlink shared tmp: %w", err)
	}
	if err := os.Rename(tmp, link); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("atomic shared rename: %w", err)
	}
	return nil
}

// RunningSharedDigest returns the digest directory the live symlink points at,
// or "" if absent / unreadable.
func (m *Manager) RunningSharedDigest(name string) string {
	dst, err := os.Readlink(m.SharedLink(name))
	if err != nil {
		return ""
	}
	// Expected: store/<digest>/<name>
	parts := strings.Split(filepath.ToSlash(dst), "/")
	if len(parts) < 3 || parts[0] != "store" {
		return ""
	}
	return "sha256:" + parts[1]
}

// RemoveSharedLink removes the live symlink for name (does not GC store entries).
func (m *Manager) RemoveSharedLink(name string) error {
	if err := sharedfile.ValidateName(name); err != nil {
		return err
	}
	link := m.SharedLink(name)
	if err := os.Remove(link); err != nil && !os.IsNotExist(err) {
		return err
	}
	return nil
}

// LinkReleaseShared creates releases/<v>/shared → ../../../shared (relative)
// so configs can resolve ./shared/<name> relative to the config directory.
func (m *Manager) LinkReleaseShared(strategy, version string) error {
	dir := m.ReleaseDir(strategy, version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("mkdir release for shared link: %w", err)
	}
	link := filepath.Join(dir, "shared")
	target := filepath.Join("..", "..", "..", "shared")
	if dst, err := os.Readlink(link); err == nil && dst == target {
		return nil
	}
	_ = os.Remove(link)
	if err := os.Symlink(target, link); err != nil {
		return fmt.Errorf("symlink release shared: %w", err)
	}
	// Ensure machine shared root exists so the relative link resolves.
	if err := os.MkdirAll(m.SharedRoot(), 0o755); err != nil {
		return fmt.Errorf("mkdir shared root: %w", err)
	}
	return nil
}

// GCShared retains the currently-linked store entry plus the last retention-1
// previous entries per shared-file name that is still desired (keepNames).
// Names absent from keepNames have all store entries removed (orphaned after
// desired removal). Never deletes the live symlink target of a kept name.
// retention <= 0 means keep everything for desired names (orphans still swept).
// keepNames may be nil (treated as empty — sweep all store names).
func (m *Manager) GCShared(retention int, keepNames map[string]struct{}) error {
	storeDir := m.SharedStoreDir()
	entries, err := os.ReadDir(storeDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	// Map name -> list of (digestDir, mtime) present in the store.
	type entry struct {
		digestDir string
		modTime   time.Time
	}
	byName := map[string][]entry{}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		digestDir := e.Name()
		sub := filepath.Join(storeDir, digestDir)
		files, err := os.ReadDir(sub)
		if err != nil {
			continue
		}
		info, _ := e.Info()
		mod := time.Time{}
		if info != nil {
			mod = info.ModTime()
		}
		for _, f := range files {
			if f.IsDir() {
				continue
			}
			name := f.Name()
			fi, err := f.Info()
			mt := mod
			if err == nil {
				mt = fi.ModTime()
			}
			byName[name] = append(byName[name], entry{digestDir: digestDir, modTime: mt})
		}
	}
	for name, list := range byName {
		if _, desired := keepNames[name]; !desired {
			// Name no longer desired: reclaim all store bytes (§3.2).
			for _, e := range list {
				_ = os.Remove(filepath.Join(storeDir, e.digestDir, name))
				_ = os.Remove(filepath.Join(storeDir, e.digestDir))
			}
			continue
		}
		if retention <= 0 {
			continue
		}
		live := digestDirName(m.RunningSharedDigest(name))
		sort.Slice(list, func(i, j int) bool {
			return list[i].modTime.After(list[j].modTime)
		})
		keep := map[string]struct{}{}
		if live != "" {
			keep[live] = struct{}{}
		}
		for _, e := range list {
			if len(keep) >= retention {
				break
			}
			keep[e.digestDir] = struct{}{}
		}
		for _, e := range list {
			if _, ok := keep[e.digestDir]; ok {
				continue
			}
			path := filepath.Join(storeDir, e.digestDir, name)
			_ = os.Remove(path)
			// Remove digest dir if empty.
			_ = os.Remove(filepath.Join(storeDir, e.digestDir))
		}
	}
	return nil
}
