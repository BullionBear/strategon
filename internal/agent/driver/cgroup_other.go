//go:build !linux

package driver

import "errors"

// CgroupRootAuto resolves the root from the agent's own cgroup (Linux only).
const CgroupRootAuto = "auto"

// ResolveCgroupRoot passes the flag through off Linux.
func ResolveCgroupRoot(flag string) (string, error) { return flag, nil }

// SetupCgroupRoot always fails off Linux: there is no cgroup v2.
func SetupCgroupRoot(root string) error { return errors.New("cgroup v2 requires linux") }

// SlotCgroupStats reports no cgroup off Linux.
func SlotCgroupStats(root, slot string) (current, peak, oomKills int64, ok bool) {
	return 0, 0, 0, false
}
