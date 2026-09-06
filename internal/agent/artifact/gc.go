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

// GCReleases deletes old release directories, keeping `keep` versions and
// up to ReleaseRetention most-recently-modified others. Current and prev
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
		extra = append(extra, ver{name: e.Name(), mod: info.ModTime()})
	}
	sort.Slice(extra, func(i, j int) bool { return extra[i].mod.After(extra[j].mod) })
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
