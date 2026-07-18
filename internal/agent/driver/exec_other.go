//go:build !linux

package driver

import (
	"errors"
	"syscall"
	"time"
)

// errUnsupported is returned by the non-Linux stub. The exec driver depends on
// Linux-specific facilities (pidfd, setsid semantics, cgroup v2); production
// runs on Linux only. This stub exists solely so `go build ./...` succeeds on
// developer machines (e.g. macOS).
var errUnsupported = errors.New("driver: exec driver requires linux")

// ExecDriver stub for non-Linux platforms.
type ExecDriver struct{ CgroupRoot string }

// NewExecDriver returns a non-functional stub on non-Linux platforms.
func NewExecDriver(cgroupRoot string) *ExecDriver { return &ExecDriver{CgroupRoot: cgroupRoot} }

func (d *ExecDriver) Start(spec StartSpec, now time.Time) (*Process, error) {
	return nil, errUnsupported
}

func (d *ExecDriver) WatchExit(p *Process, now func() time.Time) ExitInfo {
	return ExitInfo{PID: p.PID, StartTime: p.StartTime, At: now()}
}

func (d *ExecDriver) Signal(p *Process, sig syscall.Signal) error { return errUnsupported }

func (d *ExecDriver) Adopt(pid int, startTime uint64, startedAt time.Time) (*Process, error) {
	return nil, errUnsupported
}
