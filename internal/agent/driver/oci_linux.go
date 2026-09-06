//go:build linux

package driver

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// OCIDriver starts a strategy by re-execing this binary as --oci-init inside
// a user+mount+pid+uts namespace (host network). Supervision is the same
// host PID handle ExecDriver produces.
type OCIDriver struct {
	exec *ExecDriver
}

// NewOCIDriver returns an OCI driver. exec may be nil (cgroup confinement off).
func NewOCIDriver(execDrv *ExecDriver) *OCIDriver {
	return &OCIDriver{exec: execDrv}
}

func (d *OCIDriver) Start(spec StartSpec, now time.Time) (*Process, error) {
	if spec.Rootfs == "" {
		return nil, fmt.Errorf("oci: empty rootfs")
	}
	if len(spec.Argv) == 0 {
		return nil, fmt.Errorf("oci: empty argv")
	}
	uid, gid := spec.ContainerUID, spec.ContainerGID
	if gid == 0 && uid != 0 {
		gid = uid
	}

	cmd := exec.Command("/proc/self/exe", BuildInitArgs(spec)...)
	// Never leave Env nil: exec.Cmd reads that as "inherit", which would leak
	// the agent's environment into the container.
	cmd.Env = spec.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.Dir = spec.WorkDir
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: unix.CLONE_NEWUSER | unix.CLONE_NEWNS | unix.CLONE_NEWPID | unix.CLONE_NEWUTS,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: uid, HostID: os.Getuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: gid, HostID: os.Getgid(), Size: 1},
		},
		Setsid: true,
	}

	if d.exec != nil {
		if cgFD := d.exec.setupCgroup(spec); cgFD >= 0 {
			cmd.SysProcAttr.UseCgroupFD = true
			cmd.SysProcAttr.CgroupFD = cgFD
			defer unix.Close(cgFD)
		}
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("oci start: %w", err)
	}
	return attachStarted(cmd, now)
}

func (d *OCIDriver) WatchExit(p *Process, now func() time.Time) ExitInfo {
	if d.exec != nil {
		return d.exec.WatchExit(p, now)
	}
	return (&ExecDriver{}).WatchExit(p, now)
}

func (d *OCIDriver) Signal(p *Process, sig syscall.Signal) error {
	if d.exec != nil {
		return d.exec.Signal(p, sig)
	}
	return (&ExecDriver{}).Signal(p, sig)
}

func (d *OCIDriver) Adopt(pid int, startTime uint64, startedAt time.Time) (*Process, error) {
	if d.exec != nil {
		return d.exec.Adopt(pid, startTime, startedAt)
	}
	return (&ExecDriver{}).Adopt(pid, startTime, startedAt)
}
