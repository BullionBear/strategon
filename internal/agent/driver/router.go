package driver

import (
	"fmt"
	"syscall"
	"time"
)

// Router sends Start to Exec or OCI and keeps WatchExit/Signal/Adopt on Exec
// so both kinds share the same host-PID supervision model.
type Router struct {
	Exec Driver
	OCI  Driver
}

// NewRouter returns a Router. oci may be nil; OCI Start then fails clearly.
func NewRouter(execDrv, ociDrv Driver) *Router {
	return &Router{Exec: execDrv, OCI: ociDrv}
}

func (r *Router) Start(spec StartSpec, now time.Time) (*Process, error) {
	if spec.Driver == KindOCI {
		if r.OCI == nil {
			return nil, fmt.Errorf("driver: OCI not configured")
		}
		return r.OCI.Start(spec, now)
	}
	return r.Exec.Start(spec, now)
}

func (r *Router) WatchExit(p *Process, now func() time.Time) ExitInfo {
	return r.Exec.WatchExit(p, now)
}

func (r *Router) Signal(p *Process, sig syscall.Signal) error {
	return r.Exec.Signal(p, sig)
}

func (r *Router) Adopt(pid int, startTime uint64, startedAt time.Time) (*Process, error) {
	return r.Exec.Adopt(pid, startTime, startedAt)
}
