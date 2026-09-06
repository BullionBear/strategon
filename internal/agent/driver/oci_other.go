//go:build !linux

package driver

import (
	"errors"
	"syscall"
	"time"
)

// ErrOCIUnsupported is returned when the OCI driver is used off Linux.
var ErrOCIUnsupported = errors.New("driver: OCI driver requires linux")

// OCIDriver is a stub so `go build ./...` succeeds on developer machines.
type OCIDriver struct{}

// NewOCIDriver returns a non-functional stub on non-Linux platforms.
func NewOCIDriver(_ *ExecDriver) *OCIDriver { return &OCIDriver{} }

func (d *OCIDriver) Start(StartSpec, time.Time) (*Process, error) {
	return nil, ErrOCIUnsupported
}

func (d *OCIDriver) WatchExit(p *Process, now func() time.Time) ExitInfo {
	return ExitInfo{PID: p.PID, StartTime: p.StartTime, At: now()}
}

func (d *OCIDriver) Signal(*Process, syscall.Signal) error { return ErrOCIUnsupported }

func (d *OCIDriver) Adopt(int, uint64, time.Time) (*Process, error) {
	return nil, ErrOCIUnsupported
}
