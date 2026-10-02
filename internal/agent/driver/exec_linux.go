//go:build linux

package driver

import (
	"errors"
	"fmt"
	"io"
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

// sealTimeout is how long Start waits for the helper to remount cgroupfs
// and ack. A helper that never writes is killed; treating its exit as a
// successful start would crash-loop a payload that was never confined.
const sealTimeout = 5 * time.Second

// Start launches the process detached in its own session.
func (d *ExecDriver) Start(spec StartSpec, now time.Time) (*Process, error) {
	if spec.CaptureStdio && spec.PayloadLogDir == "" {
		return nil, fmt.Errorf("start %s: capture_stdio without log dir", spec.BinaryPath)
	}
	if err := checkRlimitNofile(spec.MaxOpenFiles); err != nil {
		return nil, fmt.Errorf("start %s: %w", spec.BinaryPath, err)
	}
	// A delegated cgroup is writable by this uid. The payload shares that
	// uid, so DAC cannot stop it raising memory.max or walking cgroup.procs
	// to a sibling. Confine it: user ns (uid 0 inside, mapped to this uid),
	// mount ns whose /sys/fs/cgroup is the leaf, cgroup ns rooted at the
	// leaf, and no mount/umount/setns after the cover is in place.
	seal := d.CgroupRoot != ""
	cmd := execCommand(spec, seal)
	cmd.Env = spec.Env
	cmd.Dir = spec.WorkDir
	cmd.SysProcAttr = &syscall.SysProcAttr{
		// ① Independent session/process group: agent exit does not terminate
		// the strategy (self-update prerequisite).
		Setsid: true,
	}
	if seal {
		setSealNamespaces(cmd.SysProcAttr)
	}

	// ② cgroup v2 slot. A limit that cannot be applied fails the start.
	// EXEC enters <slot>/leaf; limits are written on <slot> so they also
	// cover an adopted process that still sits directly in the slot.
	var (
		cgFD int
		err  error
	)
	if seal {
		cgFD, err = d.setupExecCgroup(spec)
	} else {
		cgFD, err = d.setupCgroup(spec)
	}
	if err != nil {
		return nil, fmt.Errorf("start %s: %w", spec.BinaryPath, err)
	}
	if cgFD >= 0 {
		cmd.SysProcAttr.UseCgroupFD = true
		cmd.SysProcAttr.CgroupFD = cgFD
		defer unix.Close(cgFD)
	}

	if seal {
		if err := startSealed(cmd); err != nil {
			return nil, fmt.Errorf("start %s: %w", spec.BinaryPath, err)
		}
	} else if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", spec.BinaryPath, err)
	}
	return attachStarted(cmd, now)
}

// execCommand builds the EXEC launch. The payload runs directly unless stdio
// capture, max_open_files, or the cgroup seal needs this binary in front of
// it. The helper applies the rlimit, seals the cgroup mount, and acks before
// the payload's first instruction.
func execCommand(spec StartSpec, seal bool) *exec.Cmd {
	if !seal && !spec.CaptureStdio && spec.MaxOpenFiles <= 0 {
		return exec.Command(spec.BinaryPath, spec.Args...)
	}
	var args []string
	if spec.CaptureStdio {
		args = buildStdioTeeArgs(spec)
	} else {
		args = append([]string{flagExecPayload, "--", spec.BinaryPath}, spec.Args...)
	}
	if seal {
		args = append([]string{flagSealCgroup}, args...)
	}
	return exec.Command("/proc/self/exe", withRlimitArgs(spec, args)...)
}

// setSealNamespaces adds the user, mount, and cgroup namespaces the
// --seal-cgroup helper needs. uid 0 inside keeps CAP_SYS_ADMIN across the
// helper's exec so it can mount the leaf cgroupfs.
func setSealNamespaces(attr *syscall.SysProcAttr) {
	attr.Cloneflags = uintptr(unix.CLONE_NEWUSER | unix.CLONE_NEWNS | unix.CLONE_NEWCGROUP)
	attr.UidMappings = []syscall.SysProcIDMap{{
		ContainerID: 0, HostID: os.Getuid(), Size: 1,
	}}
	attr.GidMappings = []syscall.SysProcIDMap{{
		ContainerID: 0, HostID: os.Getgid(), Size: 1,
	}}
}

// startSealed waits until the helper reports that the host cgroupfs is
// covered.
// On any other result the child is killed and reaped so it cannot be
// supervised as a running payload.
func startSealed(cmd *exec.Cmd) error {
	pr, pw, err := os.Pipe()
	if err != nil {
		return err
	}
	cmd.ExtraFiles = []*os.File{pw}
	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return err
	}
	pw.Close()
	defer pr.Close()
	_ = pr.SetDeadline(time.Now().Add(sealTimeout))
	buf, readErr := io.ReadAll(pr)
	text := strings.TrimSpace(string(buf))
	if readErr != nil || text != "ok" {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_, _ = cmd.Process.Wait()
		}
		if text != "" {
			return errors.New(text)
		}
		if readErr != nil {
			return fmt.Errorf("cgroup seal failed: %w", readErr)
		}
		return errors.New("cgroup seal failed")
	}
	return nil
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

// CheckLimits reports the limit errors Start would hit before forking.
// A cpu quota the kernel rejects is reported before the missing-root
// check, and a probe write uses a throwaway cgroup rather than the live
// slot: rollback reuses this spec, so the failure has to happen while the
// old process is still running.
func (d *ExecDriver) CheckLimits(spec StartSpec) error {
	if spec.MemoryBytes < 0 || spec.CPUMillicores < 0 || spec.MaxOpenFiles < 0 {
		return errors.New("limits must not be negative")
	}
	if err := checkRlimitNofile(spec.MaxOpenFiles); err != nil {
		return err
	}
	if err := ValidateCPUMillicores(spec.CPUMillicores); err != nil {
		return err
	}
	if spec.MemoryBytes == 0 && spec.CPUMillicores == 0 {
		return nil
	}
	if d.CgroupRoot == "" {
		return errNoCgroupRoot
	}
	return probeCgroupLimits(d.CgroupRoot, spec.MemoryBytes, spec.CPUMillicores)
}

// ApplyLimits rewrites the slot cgroup from spec. The process is left
// where it is: a payload in the slot and one in <slot>/leaf are both
// charged to the slot's memory.max and cpu.max.
func (d *ExecDriver) ApplyLimits(spec StartSpec) error {
	_, err := ensureSlotLimits(d.CgroupRoot, spec)
	return err
}

var errNoCgroupRoot = errors.New("limits require a cgroup root and this agent has none (--cgroup-root)")

// setupCgroup prepares the slot cgroup and returns an fd for UseCgroupFD, or
// -1 when confinement is off and the spec asks for no memory or cpu limit.
// Every payload under a root gets a slot, limited or not, so its descendants
// stay accounted to it and the unit cgroup never holds processes directly.
// OCI places the process in this cgroup. EXEC uses setupExecCgroup.
func (d *ExecDriver) setupCgroup(spec StartSpec) (int, error) {
	dir, err := ensureSlotLimits(d.CgroupRoot, spec)
	if err != nil || dir == "" {
		return -1, err
	}
	return openCgroupDir(dir)
}

// setupExecCgroup writes limits on the slot and returns an fd for the leaf
// the process is cloned into. subtree_control on the slot stays empty.
func (d *ExecDriver) setupExecCgroup(spec StartSpec) (int, error) {
	dir, err := ensureSlotLimits(d.CgroupRoot, spec)
	if err != nil || dir == "" {
		return -1, err
	}
	leaf := filepath.Join(dir, cgroupLeaf)
	if err := os.MkdirAll(leaf, 0o755); err != nil {
		return -1, fmt.Errorf("cgroup %s: %w", leaf, err)
	}
	return openCgroupDir(leaf)
}

// ensureSlotLimits creates the slot and writes memory.max and cpu.max.
// An empty root with no memory or cpu limit returns an empty dir.
func ensureSlotLimits(root string, spec StartSpec) (string, error) {
	if root == "" {
		if spec.MemoryBytes > 0 || spec.CPUMillicores > 0 {
			return "", errNoCgroupRoot
		}
		return "", nil
	}
	if spec.Strategy == "" || spec.Strategy == "." || spec.Strategy == ".." || strings.Contains(spec.Strategy, "/") {
		return "", fmt.Errorf("cgroup: invalid slot name %q", spec.Strategy)
	}
	dir := filepath.Join(root, spec.Strategy)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("cgroup %s: %w", dir, err)
	}
	if err := writeCgroupLimits(dir, spec.MemoryBytes, spec.CPUMillicores); err != nil {
		return "", fmt.Errorf("cgroup %s: %w", dir, err)
	}
	return dir, nil
}

func openCgroupDir(dir string) (int, error) {
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
