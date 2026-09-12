// Package driver abstracts how a strategy workload is executed. Exec is a bare
// process (setsid + optional cgroup v2 + pidfd). OCI is a rootless user
// namespace + pivot_root; WatchExit/Signal/Adopt stay on the exec path.
package driver

import (
	"path/filepath"
	"syscall"
	"time"
)

// Kind selects how Start launches a workload. The reconciler derives this from
// the launch artifact's type, not from desired spec.Driver.
type Kind int

const (
	KindExec Kind = iota
	KindOCI
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

	Driver Kind

	// OCI-only. Rootfs is the unpacked image. Argv is the full container
	// argv (Argv[0] is an unresolved command name — PATH lookup happens after
	// pivot_root). ImageEnv is the image Config.Env before spec overrides.
	Rootfs       string
	Argv         []string
	ImageEnv     []string
	ContainerUID int
	ContainerGID int

	// OCI binds (host paths, same path inside the container).
	WorkBind   string
	SharedBind string
	ConfigBind string // host path of the config file; empty if none

	// VolumeBinds are OCI-only host→container mounts. EXEC ignores this field.
	VolumeBinds []VolumeBind

	// CaptureStdio, when true, wraps the payload with a size-rotated tee
	// writing <PayloadLogDir>/payload.log. PayloadLogDir is the host
	// .stdio directory (StrategyDir, not WorkDir).
	CaptureStdio   bool
	PayloadLogDir  string
	PayloadVersion string
}

// VolumeBind is one OCI bind of a machine volume directory at containerPath.
type VolumeBind struct {
	Host      string
	Container string
}

// OCIInitLogName is the host-side file that captures --oci-init stderr.
// Truncated on each Start so crash loops cannot grow it without bound.
const OCIInitLogName = "oci-init.log"

// OCIInitLogPath is the host path for oci-init stderr. It sits next to
// (not inside) WorkDir so the payload cannot overwrite the log through the
// bind-mounted work directory.
func OCIInitLogPath(workDir string) string {
	return filepath.Join(filepath.Dir(workDir), OCIInitLogName)
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
	// prerequisite).
	Start(spec StartSpec, now time.Time) (*Process, error)

	// WatchExit blocks until the process exits and returns exit info. It is
	// intended to run in its own goroutine feeding the reconciler's exit
	// channel.
	WatchExit(p *Process, now func() time.Time) ExitInfo

	// Signal sends sig to the process group (pgid), covering children the
	// strategy forked.
	Signal(p *Process, sig syscall.Signal) error

	// Adopt re-attaches to an already-running process by pid, validating
	// startTime to reject PID reuse. Used after agent self-update.
	Adopt(pid int, startTime uint64, startedAt time.Time) (*Process, error)
}
