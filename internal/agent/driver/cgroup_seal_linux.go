//go:build linux

package driver

import (
	"fmt"
	"os"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// sealAckFD is the pipe the parent passed in ExtraFiles. The helper writes
// one line and closes it before exec, so Start can fail the launch instead
// of supervising a process whose cgroup is still writable.
const sealAckFD = 3

const (
	seccompDataNR   = 0
	seccompDataArch = 4
)

// sealHostCgroupFS hides the host cgroup hierarchy and then makes that
// cover permanent for this process.
//
// CLONE_NEWNS copies the parent's shared mounts, so the first step is
// MS_PRIVATE: anything mounted afterwards stays in this namespace. A new
// cgroup2 mount is rooted at the cgroup namespace root (the leaf the
// process was cloned into). Binding that over /sys/fs/cgroup hides the
// slot's memory.max and cgroup.procs. A user namespace cannot remount the
// host cgroup superblock read-only, and unmounting the cover reveals it,
// so mount, umount, setns, and the new mount API are blocked before exec.
func sealHostCgroupFS() error {
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make mounts private: %w", err)
	}
	dir, err := os.MkdirTemp("", "strategon-cg-")
	if err != nil {
		return fmt.Errorf("cgroup cover: %w", err)
	}
	if err := unix.Mount("none", dir, "cgroup2", unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		os.Remove(dir)
		return fmt.Errorf("mount leaf cgroupfs: %w", err)
	}
	if err := unix.Mount(dir, cgroupFS, "", unix.MS_BIND, ""); err != nil {
		unix.Unmount(dir, unix.MNT_DETACH)
		os.Remove(dir)
		return fmt.Errorf("cover host cgroupfs: %w", err)
	}
	var covered, leaf unix.Stat_t
	if err := unix.Stat(cgroupFS, &covered); err != nil {
		return fmt.Errorf("stat cgroup cover: %w", err)
	}
	if err := unix.Stat(dir, &leaf); err != nil {
		return fmt.Errorf("stat leaf cgroupfs: %w", err)
	}
	if covered.Dev != leaf.Dev || covered.Ino != leaf.Ino {
		return fmt.Errorf("host cgroupfs is still visible")
	}
	// The bind is its own mount. Dropping the temporary one does not
	// uncover /sys/fs/cgroup, and it has to happen before seccomp
	// blocks umount.
	if err := unix.Unmount(dir, unix.MNT_DETACH); err != nil {
		return fmt.Errorf("drop leaf mount: %w", err)
	}
	os.Remove(dir)
	if err := denyMountSyscalls(); err != nil {
		return err
	}
	return nil
}

// denyMountSyscalls installs a seccomp filter that returns EPERM for the
// syscalls that could uncover the host cgroupfs. Everything else is allowed.
// The filter is inherited across exec.
func denyMountSyscalls() error {
	arch, err := linuxAuditArch()
	if err != nil {
		return err
	}
	if err := unix.Prctl(unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	denied := []uint32{
		unix.SYS_MOUNT,
		unix.SYS_UMOUNT2,
		unix.SYS_PIVOT_ROOT,
		unix.SYS_SETNS,
		unix.SYS_OPEN_TREE,
		unix.SYS_MOVE_MOUNT,
		unix.SYS_FSOPEN,
		unix.SYS_FSCONFIG,
		unix.SYS_FSMOUNT,
		unix.SYS_FSPICK,
		unix.SYS_MOUNT_SETATTR,
	}
	filter := []unix.SockFilter{
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: seccompDataArch},
		{Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K, K: arch, Jt: 1, Jf: 0},
		{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_KILL_PROCESS},
		{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: seccompDataNR},
	}
	n := len(denied)
	for i, nr := range denied {
		filter = append(filter, unix.SockFilter{
			Code: unix.BPF_JMP | unix.BPF_JEQ | unix.BPF_K,
			K:    nr,
			Jt:   uint8(n - i),
			Jf:   0,
		})
	}
	filter = append(filter,
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ALLOW},
		unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)},
	)
	prog := unix.SockFprog{Len: uint16(len(filter)), Filter: &filter[0]}
	if err := unix.Prctl(unix.PR_SET_SECCOMP, uintptr(unix.SECCOMP_MODE_FILTER), uintptr(unsafe.Pointer(&prog)), 0, 0); err != nil {
		return fmt.Errorf("seccomp: %w", err)
	}
	runtime.KeepAlive(filter)
	runtime.KeepAlive(&prog)
	return nil
}

func linuxAuditArch() (uint32, error) {
	switch runtime.GOARCH {
	case "amd64":
		return unix.AUDIT_ARCH_X86_64, nil
	case "arm64":
		return unix.AUDIT_ARCH_AARCH64, nil
	default:
		return 0, fmt.Errorf("cgroup seal: unsupported architecture %s", runtime.GOARCH)
	}
}

// ackSeal writes the handshake line to the parent's pipe and closes it.
func ackSeal(err error) {
	f := os.NewFile(sealAckFD, "seal")
	if f == nil {
		return
	}
	if err != nil {
		fmt.Fprintf(f, "seal: %s\n", err.Error())
	} else {
		fmt.Fprint(f, "ok\n")
	}
	_ = f.Close()
}
