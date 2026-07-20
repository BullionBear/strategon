//go:build linux

package driver

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ExecDriver is the default bare-process driver. It uses setsid to detach the
// strategy into its own session/process group and a pidfd for exit
// notification. Optional cgroup v2 confinement is best-effort: if no delegated
// cgroup subtree is writable (e.g. in CI), the limits are skipped rather than
// failing the launch.
type ExecDriver struct {
	// CgroupRoot is the delegated cgroup v2 base (e.g.
	// "/sys/fs/cgroup/strategon"). Empty disables cgroup confinement.
	CgroupRoot string
}

// NewExecDriver returns an ExecDriver. cgroupRoot may be empty to disable
// cgroup confinement (the common case outside a systemd-delegated deployment).
func NewExecDriver(cgroupRoot string) *ExecDriver {
	return &ExecDriver{CgroupRoot: cgroupRoot}
}

// Start launches the process detached in its own session.
func (d *ExecDriver) Start(spec StartSpec, now time.Time) (*Process, error) {
	cmd := exec.Command(spec.BinaryPath, spec.Args...)
	cmd.Env = spec.Env
	cmd.Dir = spec.WorkDir
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// ① Independent session/process group: agent exit does not terminate
		// the strategy (self-update prerequisite).
		Setsid: true,
	}

	// ② Optional cgroup v2 confinement. Guarded: any failure degrades to an
	// unconfined launch so CI and non-delegated hosts still work.
	cgFD := d.setupCgroup(spec)
	if cgFD >= 0 {
		cmd.SysProcAttr.UseCgroupFD = true
		cmd.SysProcAttr.CgroupFD = cgFD
		defer unix.Close(cgFD)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", spec.BinaryPath, err)
	}
	pid := cmd.Process.Pid

	// ③ pidfd: a pollable exit notification. If unavailable, WatchExit falls
	// back to cmd-independent polling of /proc.
	pidfd := -1
	if fd, err := unix.PidfdOpen(pid, 0); err == nil {
		pidfd = fd
	}

	startTime := readProcStartTime(pid)

	// Release the os/exec bookkeeping so Go's runtime does not also try to wait
	// for the child (that would race our pidfd-based supervision). setsid only
	// changes the session, NOT the parent: this agent remains the process's
	// parent and must reap it when it exits (done in WatchExit) or it lingers
	// as a zombie. If the agent exits first (self-update), the still-running
	// process reparents to init, which reaps it.
	_ = cmd.Process.Release()

	return &Process{
		PID:       pid,
		StartTime: startTime,
		PGID:      pid, // setsid makes the child a group leader: pgid == pid
		StartedAt: now,
		pidfd:     pidfd,
		owned:     true, // we forked it: WatchExit reaps it
	}, nil
}

// WatchExit blocks until the process exits, then reaps it if we own it (Start-
// forked) so it does not linger as a zombie — an unreaped zombie keeps its
// /proc/<pid> entry, which makes processAlive() report it as still running and
// would stall the reconciler (no exit ever observed). It uses the pidfd when
// available, otherwise wait4 (owned) or /proc polling (adopted).
func (d *ExecDriver) WatchExit(p *Process, now func() time.Time) ExitInfo {
	code := 0
	switch {
	case p.pidfd >= 0:
		pollPidfd(p.pidfd)
		unix.Close(p.pidfd)
		p.pidfd = -1
		if p.owned {
			code = reapChild(p.PID) // process is a zombie now; wait4 returns at once
		}
	case p.owned:
		// No pidfd (kernel <5.3 or pidfd_open failed): wait4 both blocks until
		// exit and reaps the zombie in one step.
		code = reapChild(p.PID)
	default:
		// Adopted, no pidfd: not our child, so wait4 would ECHILD. Poll /proc;
		// init reaps it when it exits.
		for processAlive(p.PID, p.StartTime) {
			time.Sleep(100 * time.Millisecond)
		}
	}
	return ExitInfo{PID: p.PID, StartTime: p.StartTime, Code: code, At: now()}
}

// reapChild waits for a process this agent forked to exit and reaps it,
// clearing the zombie and returning its exit code. It is safe to call once the
// process is already a zombie (wait4 returns immediately). Returns 0 if the
// process is not our child (ECHILD) or on error; a signalled process reports -1
// via WaitStatus.ExitStatus.
func reapChild(pid int) int {
	var ws unix.WaitStatus
	for {
		wpid, err := unix.Wait4(pid, &ws, 0, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil || wpid != pid {
			return 0
		}
		return ws.ExitStatus()
	}
}

// Signal sends sig to the whole process group (negative pid).
func (d *ExecDriver) Signal(p *Process, sig syscall.Signal) error {
	if p.PGID <= 0 {
		return errors.New("driver: no pgid")
	}
	return syscall.Kill(-p.PGID, sig)
}

// Adopt re-attaches to a running process, rejecting PID reuse via startTime.
func (d *ExecDriver) Adopt(pid int, startTime uint64, startedAt time.Time) (*Process, error) {
	cur := readProcStartTime(pid)
	if cur == 0 {
		return nil, fmt.Errorf("adopt pid %d: not running", pid)
	}
	if startTime != 0 && cur != startTime {
		return nil, fmt.Errorf("adopt pid %d: starttime mismatch (pid reuse: have %d want %d)", pid, cur, startTime)
	}
	pidfd := -1
	if fd, err := unix.PidfdOpen(pid, 0); err == nil {
		pidfd = fd
	}
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		pgid = pid
	}
	return &Process{PID: pid, StartTime: cur, PGID: pgid, StartedAt: startedAt, pidfd: pidfd}, nil
}

// setupCgroup creates a per-strategy cgroup v2 subtree and writes limits,
// returning an fd for UseCgroupFD. Returns -1 on any failure (degraded mode).
func (d *ExecDriver) setupCgroup(spec StartSpec) int {
	if d.CgroupRoot == "" {
		return -1
	}
	dir := d.CgroupRoot + "/" + spec.Strategy
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return -1
	}
	if spec.MemoryBytes > 0 {
		_ = os.WriteFile(dir+"/memory.max", []byte(strconv.FormatInt(spec.MemoryBytes, 10)), 0o644)
	}
	if spec.CPUMillicores > 0 {
		// cpu.max is "quota period"; period 100000us, quota scaled by millicores.
		quota := spec.CPUMillicores * 100000 / 1000
		_ = os.WriteFile(dir+"/cpu.max", []byte(fmt.Sprintf("%d 100000", quota)), 0o644)
	}
	fd, err := unix.Open(dir, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1
	}
	return fd
}

// pollPidfd blocks until the pidfd becomes readable (process exited).
func pollPidfd(pidfd int) {
	fds := []unix.PollFd{{Fd: int32(pidfd), Events: unix.POLLIN}}
	for {
		n, err := unix.Poll(fds, -1)
		if err == unix.EINTR {
			continue
		}
		if err != nil || n > 0 {
			return
		}
	}
}

// readProcStartTime returns /proc/<pid>/stat field 22 (starttime), or 0 if the
// process is gone / unreadable.
func readProcStartTime(pid int) uint64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0
	}
	// Field 2 (comm) may contain spaces/parens; skip to the closing paren, then
	// count space-separated fields. starttime is field 22 (1-indexed), i.e. the
	// 20th field after the closing paren.
	s := string(data)
	rparen := strings.LastIndexByte(s, ')')
	if rparen < 0 || rparen+2 >= len(s) {
		return 0
	}
	fields := strings.Fields(s[rparen+2:])
	// After comm, field 3 (state) is fields[0]; starttime (field 22) is fields[19].
	if len(fields) < 20 {
		return 0
	}
	v, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return 0
	}
	return v
}

// processAlive reports whether pid is running with the expected startTime.
func processAlive(pid int, startTime uint64) bool {
	cur := readProcStartTime(pid)
	if cur == 0 {
		return false
	}
	if startTime != 0 && cur != startTime {
		return false // pid reused by a different process
	}
	return true
}
