//go:build linux

package driver

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// ExecDriver is the default bare-process driver. It uses setsid to detach the
// strategy into its own session/process group and a pidfd for exit
// notification. With a cgroup root every payload starts in its slot cgroup;
// without one, a spec that carries memory or cpu limits fails to start rather
// than running unconfined.
type ExecDriver struct {
	// CgroupRoot is the prepared cgroup v2 directory holding one cgroup per
	// slot (see SetupCgroupRoot). Empty disables cgroup confinement.
	CgroupRoot string
}

// NewExecDriver returns an ExecDriver. cgroupRoot may be empty to disable
// cgroup confinement (CI, or a host without a delegated subtree).
func NewExecDriver(cgroupRoot string) *ExecDriver {
	return &ExecDriver{CgroupRoot: cgroupRoot}
}

// Start launches the process detached in its own session.
func (d *ExecDriver) Start(spec StartSpec, now time.Time) (*Process, error) {
	if spec.CaptureStdio && spec.PayloadLogDir == "" {
		return nil, fmt.Errorf("start %s: capture_stdio without log dir", spec.BinaryPath)
	}
	if err := checkRlimitNofile(spec.MaxOpenFiles); err != nil {
		return nil, fmt.Errorf("start %s: %w", spec.BinaryPath, err)
	}
	cmd := execCommand(spec)
	cmd.Env = spec.Env
	cmd.Dir = spec.WorkDir
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// ① Independent session/process group: agent exit does not terminate
		// the strategy (self-update prerequisite).
		Setsid: true,
	}

	// ② cgroup v2 slot. A limit that cannot be applied fails the start.
	cgFD, err := d.setupCgroup(spec)
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", spec.BinaryPath, err)
	}
	if cgFD >= 0 {
		cmd.SysProcAttr.UseCgroupFD = true
		cmd.SysProcAttr.CgroupFD = cgFD
		defer unix.Close(cgFD)
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", spec.BinaryPath, err)
	}
	return attachStarted(cmd, now)
}

// execCommand builds the EXEC launch. The payload runs directly unless stdio
// capture or max_open_files needs this binary in front of it; both re-execs
// set the rlimit before the payload's first instruction.
func execCommand(spec StartSpec) *exec.Cmd {
	var args []string
	switch {
	case spec.CaptureStdio:
		args = buildStdioTeeArgs(spec)
	case spec.MaxOpenFiles > 0:
		args = append([]string{flagExecPayload, "--", spec.BinaryPath}, spec.Args...)
	default:
		return exec.Command(spec.BinaryPath, spec.Args...)
	}
	return exec.Command("/proc/self/exe", withRlimitArgs(spec, args)...)
}

// attachStarted builds a Process handle for a child this agent just Start-ed.
func attachStarted(cmd *exec.Cmd, now time.Time) (*Process, error) {
	pid := cmd.Process.Pid

	// pidfd: a pollable exit notification. If unavailable, WatchExit falls
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

// setupCgroup prepares the slot cgroup and returns an fd for UseCgroupFD, or
// -1 when confinement is off and the spec asks for no memory or cpu limit.
// Every payload under a root gets a slot, limited or not, so its descendants
// stay accounted to it and the unit cgroup never holds processes directly.
func (d *ExecDriver) setupCgroup(spec StartSpec) (int, error) {
	if d.CgroupRoot == "" {
		if spec.MemoryBytes > 0 || spec.CPUMillicores > 0 {
			return -1, errors.New("limits require a cgroup root and this agent has none (--cgroup-root)")
		}
		return -1, nil
	}
	if spec.Strategy == "" || spec.Strategy == "." || spec.Strategy == ".." || strings.Contains(spec.Strategy, "/") {
		return -1, fmt.Errorf("cgroup: invalid slot name %q", spec.Strategy)
	}
	dir := filepath.Join(d.CgroupRoot, spec.Strategy)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return -1, fmt.Errorf("cgroup %s: %w", dir, err)
	}
	if err := writeCgroupLimits(dir, spec.MemoryBytes, spec.CPUMillicores); err != nil {
		return -1, fmt.Errorf("cgroup %s: %w", dir, err)
	}
	fd, err := unix.Open(dir, unix.O_DIRECTORY|unix.O_RDONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return -1, fmt.Errorf("cgroup %s: %w", dir, err)
	}
	return fd, nil
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
