// Package volume holds validation and path helpers for machine-scoped
// named volumes used by both the control-plane API and the agent.
package volume

import (
	"fmt"
	"path"
	"path/filepath"
	"strings"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

// ReservedName is rejected so a volume cannot collide with a future
// store/ subdirectory under <base>/volumes.
const ReservedName = "lost+found"

// ValidateName rejects empty names, path separators, "." / ".." components,
// NUL bytes, and the reserved name "lost+found". Valid names are clean
// basenames written under <base>/volumes/<name>.
func ValidateName(name string) error {
	if name == "" {
		return fmt.Errorf("volume name is empty")
	}
	if name == ReservedName {
		return fmt.Errorf("volume name %q is reserved", name)
	}
	if strings.ContainsRune(name, 0) {
		return fmt.Errorf("volume name contains NUL")
	}
	if strings.ContainsAny(name, `/\`) {
		return fmt.Errorf("volume name must be a basename (no path separators)")
	}
	if name == "." || name == ".." {
		return fmt.Errorf("volume name %q is invalid", name)
	}
	return nil
}

// ValidateContainerPath checks apply-time container path rules.
// Agent-only shadowing of work/shared/config is checked separately
// because the control plane does not know --base.
func ValidateContainerPath(p string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("container_path is required")
	}
	if !path.IsAbs(p) && !strings.HasPrefix(p, "/") {
		return fmt.Errorf("container_path %q must be absolute", p)
	}
	cleaned := path.Clean(p)
	if cleaned == "/" {
		return fmt.Errorf("container_path cannot be /")
	}
	if cleaned == "/proc" || strings.HasPrefix(cleaned, "/proc/") {
		return fmt.Errorf("container_path %q collides with /proc", p)
	}
	if cleaned == "/dev" || strings.HasPrefix(cleaned, "/dev/") {
		return fmt.Errorf("container_path %q collides with /dev", p)
	}
	if cleaned == "/etc/resolv.conf" {
		return fmt.Errorf("container_path cannot be /etc/resolv.conf")
	}
	return nil
}

// PathsOverlap reports whether a and b are equal or one is a parent of the other.
func PathsOverlap(a, b string) bool {
	a = path.Clean(a)
	b = path.Clean(b)
	if a == b {
		return true
	}
	return strings.HasPrefix(a, b+"/") || strings.HasPrefix(b, a+"/")
}

// ValidateMounts checks names, required container paths, reserved paths,
// and that mounts on one assignment do not overlap.
func ValidateMounts(namesAndPaths [][2]string) error {
	seen := make(map[string]string, len(namesAndPaths))
	for _, np := range namesAndPaths {
		name, cpath := np[0], np[1]
		if err := ValidateName(name); err != nil {
			return err
		}
		if err := ValidateContainerPath(cpath); err != nil {
			return fmt.Errorf("volume %q: %w", name, err)
		}
		cleaned := path.Clean(cpath)
		if prev, ok := seen[name]; ok {
			return fmt.Errorf("duplicate volume mount %q (also at %s)", name, prev)
		}
		for other, op := range seen {
			if PathsOverlap(cleaned, op) {
				return fmt.Errorf("volume %q container_path %s overlaps %q (%s)", name, cleaned, other, op)
			}
		}
		seen[name] = cleaned
	}
	return nil
}

// EnsureInventory validates mounts and that each name exists on the machine.
func EnsureInventory(machineID string, have map[string]*pb.VolumeSpec, mounts []*pb.VolumeMount) error {
	pairs := make([][2]string, 0, len(mounts))
	for _, m := range mounts {
		pairs = append(pairs, [2]string{m.GetName(), m.GetContainerPath()})
	}
	if err := ValidateMounts(pairs); err != nil {
		return err
	}
	for _, m := range mounts {
		if have == nil || have[m.GetName()] == nil {
			return fmt.Errorf("volume %q is not on machine %s", m.GetName(), machineID)
		}
	}
	return nil
}

// ShadowsBindSame reports whether containerPath would hide a bindSame target
// (work, shared, or config) that appears inside the container at its host path.
func ShadowsBindSame(containerPath string, hostBinds ...string) bool {
	c := path.Clean(containerPath)
	for _, h := range hostBinds {
		if h == "" {
			continue
		}
		abs, err := filepath.Abs(h)
		if err != nil {
			abs = h
		}
		abs = path.Clean(filepath.ToSlash(abs))
		if PathsOverlap(c, abs) {
			return true
		}
	}
	return false
}
