package artifact

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/sharedfile"
)

// fetchedAtSuffix is a sidecar written next to each store entry with the unix
// nanos when the blob was installed. GC prefers this over mtime so rsync/
// restore cannot reorder retention (§3.5).
const fetchedAtSuffix = ".fetched_at"

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
//
// Bytes land via a sibling .partial file then rename, so a cancelled fetch
// cannot leave a truncated object at the final store path. The fast-path
// re-hash also refuses a corrupt leftover if rename was interrupted.
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
	partial := dest + ".partial"
	_ = os.Remove(partial)
	if err := m.Fetcher.Fetch(ctx, ref, partial); err != nil {
		_ = os.Remove(partial)
		return fmt.Errorf("fetch shared %s: %w", name, err)
	}
	if err := ctx.Err(); err != nil {
		_ = os.Remove(partial)
		return err
	}
	sum, err := fileSHA256(partial)
	if err != nil {
		_ = os.Remove(partial)
		return err
	}
	got := "sha256:" + sum
	if !strings.EqualFold(got, ref.GetDigest()) {
		_ = os.Remove(partial)
		return fmt.Errorf("verify shared %s: digest mismatch got %s want %s", name, got, ref.GetDigest())
	}
	if err := os.Rename(partial, dest); err != nil {
		_ = os.Remove(partial)
		return fmt.Errorf("install shared %s: %w", name, err)
	}
	_ = writeFetchedAt(dest)
	return nil
}

func writeFetchedAt(dest string) error {
	return os.WriteFile(dest+fetchedAtSuffix, []byte(strconv.FormatInt(time.Now().UnixNano(), 10)), 0o644)
}

// readFetchedAt returns the recorded install time for a store entry, or the
// zero time when the sidecar is missing/unreadable (GC falls back to mtime).
func readFetchedAt(dest string) time.Time {
	b, err := os.ReadFile(dest + fetchedAtSuffix)
	if err != nil {
		return time.Time{}
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
	if err != nil || n <= 0 {
		return time.Time{}
	}
	return time.Unix(0, n)
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
//
// Always run on Download, even when no shared files are desired yet: a later
// SetSharedFiles must work without re-fetching the release, and a dangling
// symlink is inert. Conditional linking would race "shared arrives after
// first deploy" (§3.6 — intentional, not a bug).
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
//
// Retention order prefers the recorded install time (.fetched_at sidecar),
// then mtime, then digestDir for stability — so rsync/restore mtime churn
// cannot evict the wrong previous version (§3.5).
func (m *Manager) GCShared(retention int, keepNames map[string]struct{}) error {
	storeDir := m.SharedStoreDir()
	entries, err := os.ReadDir(storeDir)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	type entry struct {
		digestDir string
		fetchedAt time.Time // recorded install time; zero → fall back to modTime
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
			if strings.HasSuffix(name, fetchedAtSuffix) || strings.HasSuffix(name, ".partial") {
				continue
			}
			path := filepath.Join(sub, name)
			fi, err := f.Info()
			mt := mod
			if err == nil {
				mt = fi.ModTime()
			}
			byName[name] = append(byName[name], entry{
				digestDir: digestDir,
				fetchedAt: readFetchedAt(path),
				modTime:   mt,
			})
		}
	}
	for name, list := range byName {
		if _, desired := keepNames[name]; !desired {
			// Name no longer desired: reclaim all store bytes (§3.2).
			for _, e := range list {
				removeSharedStoreEntry(storeDir, e.digestDir, name)
			}
			continue
		}
		if retention <= 0 {
			continue
		}
		live := digestDirName(m.RunningSharedDigest(name))
		sort.Slice(list, func(i, j int) bool {
			ai, aj := list[i], list[j]
			ti, tj := ai.fetchedAt, aj.fetchedAt
			if ti.IsZero() {
				ti = ai.modTime
			}
			if tj.IsZero() {
				tj = aj.modTime
			}
			if !ti.Equal(tj) {
				return ti.After(tj)
			}
			// Stable tie-break: lexicographic digest dir.
			return ai.digestDir > aj.digestDir
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
			removeSharedStoreEntry(storeDir, e.digestDir, name)
		}
	}
	return nil
}

func removeSharedStoreEntry(storeDir, digestDir, name string) {
	base := filepath.Join(storeDir, digestDir, name)
	_ = os.Remove(base)
	_ = os.Remove(base + fetchedAtSuffix)
	_ = os.Remove(base + ".partial")
	_ = os.Remove(filepath.Join(storeDir, digestDir)) // only if empty
}
