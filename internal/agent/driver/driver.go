// Package driver abstracts how a strategy workload is executed. The default
// (and only v1) driver is "exec": a bare process placed in its own session via
// setsid, optionally confined by a cgroup v2 subtree, and monitored via a
// Linux pidfd. See RECONCILER.md §6 and SAFETY.md §6.
//
// The abstraction exists so an OCI driver can be added later (ARCHITECTURE
// §14) without touching the reconciler, which only depends on this interface.
package driver

import (
	"syscall"
	"time"
)

// StartSpec describes a process to launch. It is intentionally decoupled from
// the protobuf types so the driver has no dependency on the wire schema.
type StartSpec struct {
	Strategy   string
	BinaryPath string
	Args       []string
	Env        []string
	WorkDir    string

	// Resource limits (0 = unset). Applied via a cgroup v2 subtree when the
	// environment supports delegation; otherwise best-effort skipped.
	CPUMillicores int64
	MemoryBytes   int64
	MaxOpenFiles  int32
}

// Process is a handle to a supervised process.
//
// StartTime is /proc/<pid>/stat field 22 (starttime in clock ticks) and is
// compared on re-adoption and on exit notifications to defend against PID
// reuse: a (pid, startTime) pair uniquely identifies a process instance.
type Process struct {
	PID       int
	StartTime uint64
	PGID      int
	StartedAt time.Time

	// pidfd is a Linux file descriptor (>=0) that becomes readable when the
	// process exits. -1 when unavailable.
	pidfd int

	// owned is true when this agent forked the process (Start), meaning it is
	// the OS parent and must reap the exit to avoid a zombie. False for
	// Adopt-ed processes (re-attached after self-update): those are children of
	// init, which reaps them, and wait4 here would return ECHILD.
	owned bool
}

// Pidfd exposes the raw pidfd for callers that need to poll it directly.
func (p *Process) Pidfd() int { return p.pidfd }

// ExitInfo is reported when a supervised process exits.
type ExitInfo struct {
	PID       int
	StartTime uint64
	Code      int
	At        time.Time
}

// Driver launches and supervises strategy processes.
type Driver interface {
	// Start forks/execs the workload in its own session (setsid) and returns a
	// handle. The process is NOT killed when the agent exits (self-update
	// prerequisite, SAFETY §6 / RECONCILER §7).
	Start(spec StartSpec, now time.Time) (*Process, error)

	// WatchExit blocks until the process exits and returns exit info. It is
	// intended to run in its own goroutine feeding the reconciler's exit
	// channel.
	WatchExit(p *Process, now func() time.Time) ExitInfo

	// Signal sends sig to the process group (pgid), covering children the
	// strategy forked.
	Signal(p *Process, sig syscall.Signal) error

	// Adopt re-attaches to an already-running process by pid, validating
	// startTime to reject PID reuse. Used after agent self-update
	// (RECONCILER §10).
	Adopt(pid int, startTime uint64, startedAt time.Time) (*Process, error)
}
