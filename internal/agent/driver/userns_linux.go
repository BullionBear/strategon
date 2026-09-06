//go:build linux

package driver

import (
	"os"
	"os/exec"
	"syscall"

	"golang.org/x/sys/unix"
)

var preferSelfExeProbe bool

// PreferSelfExeProbe makes UserNSAvailable re-exec this binary with
// --oci-probe. cmd/agent calls this so a production agent is not dependent
// on /bin/true. Tests leave it off so `go test` binaries are never re-exec'd.
func PreferSelfExeProbe() { preferSelfExeProbe = true }

// UserNSAvailable reports whether an unprivileged user namespace with a
// single-UID map can be created. The probe always forks a child — a
// multithreaded Go process cannot unshare(CLONE_NEWUSER) itself.
func UserNSAvailable() bool {
	cmd := probeCommand()
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: unix.CLONE_NEWUSER,
		UidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getuid(), Size: 1},
		},
		GidMappings: []syscall.SysProcIDMap{
			{ContainerID: 0, HostID: os.Getgid(), Size: 1},
		},
	}
	return cmd.Run() == nil
}

func probeCommand() *exec.Cmd {
	if preferSelfExeProbe {
		return exec.Command("/proc/self/exe", flagOCIProbe)
	}
	if p, err := exec.LookPath("true"); err == nil {
		return exec.Command(p)
	}
	return exec.Command("/bin/true")
}

// MaybeRunOCIHelper intercepts --oci-init / --oci-probe before the agent
// required-flag checks. Returns true if this process should not continue as
// the agent (the helper already os.Exit'd).
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
	}
	return false
}
