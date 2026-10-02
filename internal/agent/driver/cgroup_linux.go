//go:build linux

package driver

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

const (
	cgroupFS = "/sys/fs/cgroup"

	// CgroupRootAuto resolves the root to <own cgroup's parent>/strategies,
	// i.e. a sibling of the DelegateSubgroup the agent runs in.
	CgroupRootAuto = "auto"

	// cgroupUnassigned holds processes found in the root's parent at startup:
	// payloads left behind by an agent that ran before DelegateSubgroup. The
	// parent must be empty before it can enable controllers for its children.
	// It and the probe cgroup are siblings of the root, not slots under it, so
	// no slot name can collide with them.
	cgroupUnassigned = "_unassigned"
	cgroupProbe      = "_probe"
	// cgroupLeaf is the EXEC process's cgroup. Limits stay on the slot
	// (its parent) so an adopted process still sitting in the slot and a
	// new process in the leaf share one memory.max. The slot's
	// subtree_control stays empty: enabling a controller there would
	// reject processes that already live in the slot.
	cgroupLeaf = "leaf"
	// cgroupLimitProbe is a throwaway sibling of the strategies root.
	// CheckLimits writes a candidate limit here before drain. It is not a
	// slot, so the OOM sampler's baseline of the strategies directory
	// never sees it.
	cgroupLimitProbe = "_limitprobe"
)

// ResolveCgroupRoot turns the --cgroup-root flag into an absolute cgroup v2
// directory. Empty stays empty (confinement off).
func ResolveCgroupRoot(flag string) (string, error) {
	if flag != CgroupRootAuto {
		return flag, nil
	}
	own, err := ownCgroup()
	if err != nil {
		return "", err
	}
	if own == "/" {
		return "", errors.New("cgroup-root auto: agent runs in the root cgroup (no delegated subtree)")
	}
	// Without DelegateSubgroup= (systemd < 254 ignores it) the agent sits in
	// the unit cgroup itself, and its parent is a slice it must not touch.
	for _, unit := range []string{".service", ".scope", ".slice"} {
		if strings.HasSuffix(own, unit) {
			return "", fmt.Errorf("cgroup-root auto: agent runs in unit cgroup %s, not a subgroup of it (needs systemd DelegateSubgroup=, systemd >= 254)", own)
		}
	}
	return filepath.Join(cgroupFS, filepath.Dir(own), "strategies"), nil
}

// ownCgroup returns this process's cgroup v2 path from /proc/self/cgroup.
func ownCgroup() (string, error) {
	f, err := os.Open("/proc/self/cgroup")
	if err != nil {
		return "", err
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		if rest, ok := strings.CutPrefix(sc.Text(), "0::"); ok {
			return rest, nil
		}
	}
	if err := sc.Err(); err != nil {
		return "", err
	}
	return "", errors.New("no cgroup v2 entry in /proc/self/cgroup (hybrid or v1 hierarchy)")
}

// SetupCgroupRoot prepares root so every slot under it can carry memory and
// cpu limits, then proves a child can actually be cloned into it. Any error
// means limits cannot be honored and the caller must not advertise them.
//
// The parent must be writable by this user (systemd Delegate=) and must not
// hold the agent itself (DelegateSubgroup=): a cgroup with processes cannot
// enable controllers for its children.
func SetupCgroupRoot(root string) error {
	if root == "" {
		return errors.New("no cgroup root")
	}
	var st unix.Statfs_t
	if err := unix.Statfs(cgroupFS, &st); err != nil || st.Type != unix.CGROUP2_SUPER_MAGIC {
		return fmt.Errorf("%s is not a cgroup v2 mount", cgroupFS)
	}
	parent := filepath.Dir(root)
	if own, err := ownCgroup(); err == nil && filepath.Join(cgroupFS, own) == parent {
		return fmt.Errorf("agent runs in %s, the parent of the cgroup root; run it in a subgroup (systemd DelegateSubgroup=)", parent)
	}
	if err := os.MkdirAll(root, 0o755); err != nil {
		return err
	}
	if err := evacuateCgroup(parent, filepath.Join(parent, cgroupUnassigned)); err != nil {
		return err
	}
	for _, dir := range []string{parent, root} {
		if err := enableControllers(dir); err != nil {
			return err
		}
	}
	return probeCgroupPlacement(filepath.Join(parent, cgroupProbe))
}

// enableControllers turns on memory and cpu for dir's children. pids is
// best-effort: no limit depends on it.
func enableControllers(dir string) error {
	ctl := filepath.Join(dir, "cgroup.subtree_control")
	for _, c := range []string{"memory", "cpu"} {
		if err := os.WriteFile(ctl, []byte("+"+c), 0o644); err != nil {
			return fmt.Errorf("enable %s in %s: %w", c, ctl, err)
		}
	}
	_ = os.WriteFile(ctl, []byte("+pids"), 0o644)
	return nil
}

// evacuateCgroup moves every process in from into to, so from may enable
// controllers. Processes that exit mid-move are ignored.
func evacuateCgroup(from, to string) error {
	pids, err := cgroupProcs(from)
	if err != nil || len(pids) == 0 {
		return err
	}
	if err := os.MkdirAll(to, 0o755); err != nil {
		return err
	}
	for _, pid := range pids {
		err := os.WriteFile(filepath.Join(to, "cgroup.procs"), []byte(strconv.Itoa(pid)), 0o644)
		if err != nil && !errors.Is(err, syscall.ESRCH) {
			return fmt.Errorf("move pid %d out of %s: %w", pid, from, err)
		}
	}
	return nil
}

func cgroupProcs(dir string) ([]int, error) {
	b, err := os.ReadFile(filepath.Join(dir, "cgroup.procs"))
	if err != nil {
		return nil, err
	}
	var out []int
	for f := range strings.FieldsSeq(string(b)) {
		if pid, err := strconv.Atoi(f); err == nil {
			out = append(out, pid)
		}
	}
	return out, nil
}

// probeCgroupPlacement clones a throwaway child into dir with a memory limit,
// the same way Start places a payload. Writing the limit and migrating the
// child are separate permission checks; both must pass.
func probeCgroupPlacement(dir string) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	defer os.Remove(dir)
	if err := writeCgroupLimits(dir, 64<<20, 0); err != nil {
		return err
	}
	fd, err := unix.Open(dir, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	cmd := probeCommand(flagOCIProbe)
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: fd}
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("clone into %s: %w", dir, err)
	}
	return nil
}

// writeCgroupLimits sets memory.max and cpu.max on a slot. Zero writes "max"
// so a limit removed from the spec is lifted on the next start.
func writeCgroupLimits(dir string, memoryBytes, cpuMillicores int64) error {
	if memoryBytes < 0 {
		return fmt.Errorf("memory_bytes %d must not be negative", memoryBytes)
	}
	cpu, err := cpuMaxFile(cpuMillicores)
	if err != nil {
		return err
	}
	mem := "max"
	if memoryBytes > 0 {
		mem = strconv.FormatInt(memoryBytes, 10)
	}
	if err := os.WriteFile(filepath.Join(dir, "memory.max"), []byte(mem), 0o644); err != nil {
		return fmt.Errorf("memory.max: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cpu.max"), []byte(cpu), 0o644); err != nil {
		return fmt.Errorf("cpu.max: %w", err)
	}
	return nil
}

// probeCgroupLimits writes the candidate limits into a fresh sibling of
// root and removes it. A value the kernel rejects fails here, before
// deploy drains the running process or rewrites the live slot.
func probeCgroupLimits(root string, memoryBytes, cpuMillicores int64) error {
	parent := filepath.Dir(root)
	dir, err := os.MkdirTemp(parent, cgroupLimitProbe+"-")
	if err != nil {
		return fmt.Errorf("limit probe: %w", err)
	}
	defer os.Remove(dir)
	if err := writeCgroupLimits(dir, memoryBytes, cpuMillicores); err != nil {
		return fmt.Errorf("limit probe: %w", err)
	}
	return nil
}

// SlotCgroupStats reads memory accounting for a slot cgroup. ok is false when
// the slot has no cgroup (never started under this root). peak is 0 on
// kernels without memory.peak (before 5.19).
func SlotCgroupStats(root, slot string) (current, peak, oomKills int64, ok bool) {
	if root == "" {
		return 0, 0, 0, false
	}
	dir := filepath.Join(root, slot)
	current, err := readCgroupInt(filepath.Join(dir, "memory.current"))
	if err != nil {
		return 0, 0, 0, false
	}
	peak, _ = readCgroupInt(filepath.Join(dir, "memory.peak"))
	if b, err := os.ReadFile(filepath.Join(dir, "memory.events")); err == nil {
		for line := range strings.SplitSeq(string(b), "\n") {
			if v, found := strings.CutPrefix(line, "oom_kill "); found {
				oomKills, _ = strconv.ParseInt(strings.TrimSpace(v), 10, 64)
			}
		}
	}
	return current, peak, oomKills, true
}

func readCgroupInt(path string) (int64, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(strings.TrimSpace(string(b)), 10, 64)
}
