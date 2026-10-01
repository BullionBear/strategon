//go:build linux

package artifact

import (
	"os"
	"strconv"

	"golang.org/x/sys/unix"
)

func statKey(path string) (rootKey, bool) {
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return rootKey{}, false
	}
	return rootKey{dev: uint64(st.Dev), ino: uint64(st.Ino)}, true
}

// scanProcRoots maps each live process's root inode to the pids using it.
// stat of /proc/<pid>/root follows the magic link to that process's root.
// EACCES and ESRCH are skipped: a process we cannot see is not evidence it
// uses a release, and a pid that exits mid-scan is already gone.
func scanProcRoots() map[rootKey][]int {
	out := map[rootKey][]int{}
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return out
	}
	for _, e := range entries {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid <= 0 {
			continue
		}
		key, ok := statKey("/proc/" + e.Name() + "/root")
		if !ok {
			continue
		}
		out[key] = append(out[key], pid)
	}
	return out
}
