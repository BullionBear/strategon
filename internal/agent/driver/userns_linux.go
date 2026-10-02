//go:build linux

package driver

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

// ociCloneflags is the namespace set a default OCI child is created with.
// UserNSAvailable probes exactly this set. Host-PID mode clears CLONE_NEWPID
// at Start and is probed separately by HostPIDAvailable: a probe that asks
// for less than Start needs can report a host as capable where Start then
// fails, which is what the control plane's capability gate exists to prevent.
const ociCloneflags = unix.CLONE_NEWUSER | unix.CLONE_NEWNS | unix.CLONE_NEWPID | unix.CLONE_NEWUTS

// ociSysProcAttr builds the clone attributes for an OCI child mapping a single
// container uid/gid onto this process's own ids. hostPID drops CLONE_NEWPID
// so the child stays in the host PID namespace.
func ociSysProcAttr(uid, gid int, hostPID bool) *syscall.SysProcAttr {
	flags := uintptr(ociCloneflags)
	if hostPID {
		flags &^= uintptr(unix.CLONE_NEWPID)
	}
	return &syscall.SysProcAttr{
		Cloneflags: flags,
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
	cmd := probeCommand(flagOCIProbe)
	cmd.SysProcAttr = ociSysProcAttr(0, 0, false)
	return cmd.Run() == nil
}

// HostPIDAvailable reports whether this process can start an OCI child in the
// host PID namespace and recursively bind the host /proc. The child re-execs
// /proc/self/exe, which performs the bind inside its own mount namespace.
// Clone-without-NEWPID alone would pass on a kernel that then refuses the bind.
func HostPIDAvailable() bool {
	cmd := probeCommand(flagOCIProbeHostPID)
	cmd.SysProcAttr = ociSysProcAttr(0, 0, true)
	return cmd.Run() == nil
}

// runHostPIDProbe runs inside the host-pid clone. Make the mount tree private
// before the bind so a shared / does not leak the probe mount back to the host.
func runHostPIDProbe() int {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return 1
	}
	dir, err := os.MkdirTemp("", "strategon-hostpid-")
	if err != nil {
		return 1
	}
	defer os.RemoveAll(dir)
	if err := unix.Mount("/proc", dir, "", unix.MS_BIND|unix.MS_REC, ""); err != nil {
		return 1
	}
	_ = unix.Unmount(dir, unix.MNT_DETACH)
	return 0
}

// probeCommand re-execs this binary with a probe flag; MaybeRunOCIHelper
// answers it before any other startup work. A test binary that probes must
// call MaybeRunOCIHelper from TestMain for the same reason.
func probeCommand(flag string) *exec.Cmd {
	return exec.Command("/proc/self/exe", flag)
}

// MaybeRunOCIHelper intercepts --oci-init / --oci-probe / --stdio-tee /
// --exec-payload before the agent required-flag checks, after applying an
// optional leading --rlimit-nofile and --seal-cgroup. The seal acks on fd 3
// and closes it before a stdio tee forks, so the payload does not inherit
// the pipe. Returns true if this process should not continue as the agent
// (the helper already os.Exit'd).
func MaybeRunOCIHelper() bool {
	if len(os.Args) < 2 {
		return false
	}
	args := os.Args[1:]
	if val, ok := strings.CutPrefix(args[0], flagRlimitNofile+"="); ok {
		if err := applyRlimitNofile(val); err != nil {
			fmt.Fprintln(os.Stderr, "rlimit:", err)
			// Start is waiting on the seal ack; report the real cause
			// instead of a bare "cgroup seal failed".
			if len(args) > 1 && args[1] == flagSealCgroup {
				ackSeal(fmt.Errorf("rlimit: %w", err))
			}
			os.Exit(126)
		}
		args = args[1:]
		if len(args) == 0 {
			os.Exit(2)
		}
	}
	if len(args) > 0 && args[0] == flagSealCgroup {
		err := sealHostCgroupFS()
		ackSeal(err)
		if err != nil {
			os.Exit(126)
		}
		args = args[1:]
		if len(args) == 0 {
			os.Exit(2)
		}
	}
	switch args[0] {
	case flagOCIProbe:
		os.Exit(0)
		return true
	case flagOCIProbeHostPID:
		os.Exit(runHostPIDProbe())
		return true
	case flagOCIInit:
		os.Exit(runOCIInit(args[1:]))
		return true
	case flagStdioTee:
		os.Exit(runStdioTeeFromArgs(args[1:]))
		return true
	case flagExecPayload:
		os.Exit(runExecPayload(args[1:]))
		return true
	}
	if len(args) != len(os.Args)-1 {
		os.Exit(2) // --rlimit-nofile without a helper: never continue as the agent
	}
	return false
}
