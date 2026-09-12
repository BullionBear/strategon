//go:build linux

package driver

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

// ociCloneflags is the namespace set every OCI child is created with. The
// probe and OCIDriver.Start share it: a probe that asks for less than Start
// needs can report a host as OCI-capable where Start then fails, which is
// exactly what the control plane's capability gate exists to prevent.
const ociCloneflags = unix.CLONE_NEWUSER | unix.CLONE_NEWNS | unix.CLONE_NEWPID | unix.CLONE_NEWUTS

// ociSysProcAttr builds the clone attributes for an OCI child mapping a single
// container uid/gid onto this process's own ids.
func ociSysProcAttr(uid, gid int) *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		Cloneflags: ociCloneflags,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: uid, HostID: os.Getuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: gid, HostID: os.Getgid(), Size: 1},
		},
		Setsid: true,
	}
}

// UserNSAvailable reports whether this process can start an OCI child. The
// probe always forks — a multithreaded Go process cannot unshare(CLONE_NEWUSER)
// itself — and re-execs /proc/self/exe with the same namespaces Start uses.
// Probing anything weaker (a plain /bin/true with CLONE_NEWUSER only) passes on
// hosts where re-execing /proc/self/exe is refused, e.g. an Ubuntu 24.04 kernel
// with apparmor_restrict_unprivileged_userns=1.
func UserNSAvailable() bool {
	cmd := probeCommand()
	cmd.SysProcAttr = ociSysProcAttr(0, 0)
	return cmd.Run() == nil
}

// probeCommand re-execs this binary; MaybeRunOCIHelper answers --oci-probe by
// exiting 0 before any other startup work. A test binary that probes must call
// MaybeRunOCIHelper from TestMain for the same reason.
func probeCommand() *exec.Cmd {
	return exec.Command("/proc/self/exe", flagOCIProbe)
}

// MaybeRunOCIHelper intercepts --oci-init / --oci-probe / --stdio-tee before
// the agent required-flag checks. Returns true if this process should not
// continue as the agent (the helper already os.Exit'd).
func MaybeRunOCIHelper() bool {
	if len(os.Args) < 2 {
		return false
	}
	switch os.Args[1] {
	case flagOCIProbe:
		os.Exit(0)
		return true
	case flagOCIInit:
		os.Exit(runOCIInit(os.Args[2:]))
		return true
	case flagStdioTee:
		os.Exit(runStdioTeeFromArgs(os.Args[2:]))
		return true
	}
	return false
}
