package artifact

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const defaultReleaseRetention = 3

func (m *Manager) retention() int {
	if m.ReleaseRetention <= 0 {
		return defaultReleaseRetention
	}
	return m.ReleaseRetention
}

// ReleaseStampPath is the sidecar recording when a release first landed.
// Ranking by directory mtime would be wrong: any later write into an existing
// release (a config re-fetch in Download, LinkReleaseShared re-creating the
// shared symlink) bumps it, so a re-touched old release could outrank a newer
// one and survive GC in its place.
func ReleaseStampPath(dir string) string { return filepath.Join(dir, fetchedAtSuffix) }

// stampRelease records the install time once, on first landing. Re-downloads
// of an already-present release must not reorder retention.
func stampRelease(dir string) {
	p := ReleaseStampPath(dir)
	if _, err := os.Stat(p); err == nil {
		return
	}
	_ = writeFetchedAtPath(p)
}

// releaseInstalledAt prefers the sidecar and falls back to mtime for releases
// written by an older agent.
func releaseInstalledAt(dir string, info os.FileInfo) time.Time {
	if t := readFetchedAtPath(ReleaseStampPath(dir)); !t.IsZero() {
		return t
	}
	return info.ModTime()
}

// GCReleases deletes old release directories, keeping `keep` versions and
// up to ReleaseRetention most-recently-installed others. Current and prev
// must be listed in keep so rollback stays O(1) for those versions.
func (m *Manager) GCReleases(strategy string, keep []string) error {
	root := filepath.Join(m.StrategyDir(strategy), "releases")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	keepSet := map[string]struct{}{}
	for _, k := range keep {
		if k != "" {
			keepSet[k] = struct{}{}
		}
	}
	type ver struct {
		name string
		mod  time.Time
	}
	var extra []ver
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, ok := keepSet[e.Name()]; ok {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		extra = append(extra, ver{
			name: e.Name(),
			mod:  releaseInstalledAt(filepath.Join(root, e.Name()), info),
		})
	}
	// Stable with an explicit tiebreak: several releases can share a timestamp
	// (same mtime tick), and an unstable sort would then drop an arbitrary one.
	sort.SliceStable(extra, func(i, j int) bool {
		if extra[i].mod.Equal(extra[j].mod) {
			return extra[i].name > extra[j].name
		}
		return extra[i].mod.After(extra[j].mod)
	})
	// Retention includes keep+extra. Drop oldest extras beyond the budget.
	budget := m.retention() - len(keepSet)
	if budget < 0 {
		budget = 0
	}
	if len(extra) <= budget {
		return nil
	}
	for _, v := range extra[budget:] {
		if err := os.RemoveAll(filepath.Join(root, v.name)); err != nil {
			return fmt.Errorf("gc release %s: %w", v.name, err)
		}
	}
	return nil
}
