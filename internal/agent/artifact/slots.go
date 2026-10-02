package artifact

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/bullionbear/strategon/internal/agent/filebrowse"
)

// Reserved first-level names under --base. These are never strategy slots.
const (
	ReservedShared  = "shared"
	ReservedVolumes = "volumes"
	ReservedAgent   = "agent"
)

// ReservedBaseName reports whether name is a machine-level directory, not a slot.
func ReservedBaseName(name string) bool {
	switch name {
	case ReservedShared, ReservedVolumes, ReservedAgent:
		return true
	}
	return false
}

// ListStrategyDirs returns first-level strategy slot names under Base,
// skipping reserved machine dirs and names that fail ValidateStrategy.
func (m *Manager) ListStrategyDirs() ([]string, error) {
	if m == nil || m.Base == "" {
		return nil, nil
	}
	entries, err := os.ReadDir(m.Base)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() {
			// A symlink to a directory: IsDir is false (Lstat). Treat a
			// symlink-to-dir as a slot only if it is a real directory after
			// the reserved-name filter — we do not follow. Skip non-dirs.
			continue
		}
		name := e.Name()
		if ReservedBaseName(name) {
			continue
		}
		if err := filebrowse.ValidateStrategy(name); err != nil {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)
	return names, nil
}

// DirSize sums file sizes under root. It does not follow symlinks
// (current and shared would otherwise double-count).
func DirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		info, infoErr := d.Info()
		if infoErr != nil {
			if os.IsNotExist(infoErr) {
				return nil
			}
			return infoErr
		}
		if info.Mode()&os.ModeSymlink != 0 {
			total += info.Size()
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return total, err
	}
	return total, nil
}

// StrategyDirSize is DirSize of <base>/<strategy>.
func (m *Manager) StrategyDirSize(strategy string) (int64, error) {
	return DirSize(m.StrategyDir(strategy))
}

// RemoveStrategyDir deletes the whole strategy slot. Rejects reserved and
// invalid names. Missing dirs succeed. A release whose rootfs is still some
// process's root — including a setsid grandchild whose supervised parent
// has already exited — is left in place. Deleting that directory makes the
// process's root empty and the next exec of a relative path can fork without
// bound. Nothing is removed when any version is busy.
func (m *Manager) RemoveStrategyDir(strategy string) error {
	if err := filebrowse.ValidateStrategy(strategy); err != nil {
		return err
	}
	if ReservedBaseName(strategy) {
		return fmt.Errorf("reserved base name %q", strategy)
	}
	if err := m.rootfsBusy(strategy); err != nil {
		return err
	}
	if err := os.RemoveAll(m.StrategyDir(strategy)); err != nil {
		return fmt.Errorf("remove strategy dir %s: %w", strategy, err)
	}
	return nil
}

// rootfsBusy reports versions whose rootfs inode is a live process root.
// A missing releases directory is not busy. Any other read error refuses
// the delete.
func (m *Manager) rootfsBusy(strategy string) error {
	root := filepath.Join(m.StrategyDir(strategy), "releases")
	entries, err := os.ReadDir(root)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return err
	}
	users := m.releaseUserLookup()
	var parts []string
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pids := users(filepath.Join(root, e.Name(), "rootfs"))
		if len(pids) == 0 {
			continue
		}
		parts = append(parts, fmt.Sprintf("%s used by pid %s", e.Name(), formatPIDs(pids)))
	}
	if len(parts) == 0 {
		return nil
	}
	sort.Strings(parts)
	return fmt.Errorf("refusing to delete %s: rootfs still in use (%s)", strategy, strings.Join(parts, "; "))
}

func formatPIDs(pids []int) string {
	parts := make([]string, len(pids))
	for i, pid := range pids {
		parts[i] = strconv.Itoa(pid)
	}
	return strings.Join(parts, ",")
}
