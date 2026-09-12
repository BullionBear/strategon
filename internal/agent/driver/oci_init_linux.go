//go:build linux

package driver

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

func runOCIInit(args []string) int {
	ia, err := parseInitFlag(args)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		return 2
	}
	code, err := applyRootfs(ia)
	if err != nil {
		fmt.Fprintln(os.Stderr, "oci-init:", err)
		if code != 0 {
			return code
		}
		return 1
	}
	return code
}

func applyRootfs(ia InitArgs) (int, error) {
	if ia.Rootfs == "" {
		return 1, fmt.Errorf("missing --oci-rootfs")
	}
	rootfs, err := filepath.Abs(ia.Rootfs)
	if err != nil {
		return 1, err
	}

	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return 1, fmt.Errorf("make-rprivate: %w", err)
	}
	if err := unix.Mount(rootfs, rootfs, "", unix.MS_BIND, ""); err != nil {
		return 1, fmt.Errorf("bind rootfs: %w", err)
	}
	oldroot := filepath.Join(rootfs, ".oldroot")
	if err := os.MkdirAll(oldroot, 0o700); err != nil {
		return 1, fmt.Errorf("mkdir .oldroot: %w", err)
	}

	if err := bindSame(rootfs, ia.Work, true); err != nil {
		return 1, fmt.Errorf("bind work: %w", err)
	}
	if err := bindSame(rootfs, ia.Shared, true); err != nil {
		return 1, fmt.Errorf("bind shared: %w", err)
	}
	if ia.Config != "" {
		if err := bindConfig(rootfs, ia.Config); err != nil {
			return 1, fmt.Errorf("bind config: %w", err)
		}
	}
	if ia.LogDir != "" {
		if err := bindSame(rootfs, ia.LogDir, true); err != nil {
			return 1, fmt.Errorf("bind stdio log: %w", err)
		}
	}
	for _, p := range []string{"/etc/resolv.conf", "/dev/null", "/dev/zero", "/dev/urandom"} {
		if err := bindSame(rootfs, p, false); err != nil {
			// resolv.conf / devices may be missing in some test roots; skip if host path absent.
			if os.IsNotExist(err) {
				continue
			}
			return 1, fmt.Errorf("bind %s: %w", p, err)
		}
	}
	for _, b := range ia.Volumes {
		if err := bindVolume(rootfs, b.Host, b.Container); err != nil {
			return 1, fmt.Errorf("bind volume %s: %w", b.Container, err)
		}
	}

	// procfs must be mounted before pivot_root, while the host's /proc is
	// still in this mount namespace. The kernel's mount_too_revealing() only
	// lets an unprivileged user namespace mount proc when a fully visible
	// procfs already exists to compare against; detaching the old root below
	// removes the last one, so mounting afterwards is always EPERM.
	// MS_NOSUID|MS_NODEV|MS_NOEXEC matches the flags the host mount is locked
	// with — a laxer new mount fails the same check.
	procTarget := filepath.Join(rootfs, "proc")
	if err := os.MkdirAll(procTarget, 0o755); err != nil {
		return 1, err
	}
	if err := unix.Mount("proc", procTarget, "proc",
		unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return 1, fmt.Errorf("mount proc: %w", err)
	}

	if err := unix.PivotRoot(rootfs, oldroot); err != nil {
		return 1, fmt.Errorf("pivot_root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return 1, err
	}
	if err := unix.Unmount("/.oldroot", unix.MNT_DETACH); err != nil {
		return 1, fmt.Errorf("unmount oldroot: %w", err)
	}
	_ = os.Remove("/.oldroot")

	cwd := ia.CWD
	if cwd == "" {
		cwd = ia.Work
	}
	if cwd != "" {
		if err := os.MkdirAll(cwd, 0o755); err != nil {
			return 1, fmt.Errorf("mkdir cwd: %w", err)
		}
		if err := os.Chdir(cwd); err != nil {
			return 1, fmt.Errorf("chdir %s: %w", cwd, err)
		}
	}

	// Open the host .stdio bind before /tmp is covered by tmpfs. Tests (and
	// a --base under /tmp) place work + .stdio there; a path lookup after
	// the mount would create a tmpfs shadow or fail chdir in os/exec.
	var rot *SizeRotator
	if ia.LogDir != "" {
		var err error
		rot, err = OpenSizeRotator(ia.LogDir, PayloadLogName, PayloadLogMaxBytes, PayloadLogArchives)
		if err != nil {
			return 1, fmt.Errorf("stdio log: %w", err)
		}
		defer rot.Close()
	}

	if err := os.MkdirAll("/tmp", 0o1777); err != nil {
		return 1, fmt.Errorf("mkdir /tmp: %w", err)
	}
	if err := unix.Mount("tmpfs", "/tmp", "tmpfs", unix.MS_NOSUID|unix.MS_NODEV, "mode=1777"); err != nil {
		return 1, fmt.Errorf("mount tmpfs /tmp: %w", err)
	}
	// Remount after tmpfs so /tmp stays writable. Include nosuid/nodev so a
	// remount does not drop those locked flags (EPERM in a rootless userns,
	// same class as mount proc). Do not set MS_NOEXEC: the payload lives on
	// this rootfs and must be executable.
	if err := unix.Mount("", "/", "", unix.MS_REMOUNT|unix.MS_BIND|unix.MS_RDONLY|unix.MS_NOSUID|unix.MS_NODEV, ""); err != nil {
		return 1, fmt.Errorf("remount rootfs ro: %w", err)
	}

	if len(ia.Argv) == 0 {
		return 1, fmt.Errorf("empty argv")
	}
	bin, err := lookPathAfterPivot(ia.Argv[0], os.Getenv("PATH"))
	if err != nil {
		return 1, err
	}
	if rot != nil {
		// Inherit the cwd inode pinned above. Do not pass Dir: os/exec
		// would look the path up again after /tmp is gone.
		return runStdioTee(stdioTeeOpts{
			Rot:     rot,
			Version: ia.LogVer,
			Argv:    append([]string{bin}, ia.Argv[1:]...),
			Env:     os.Environ(),
		})
	}
	// Preserve the oci-init.log FD across discardStdio so a failed exec
	// (ENOENT missing interpreter, ENOEXEC wrong arch) can still be written
	// to the host log. CLOEXEC closes the dup on a successful exec.
	logFD, dupErr := unix.FcntlInt(2, unix.F_DUPFD_CLOEXEC, 3)
	// Drop the inherited oci-init.log FD so the payload's stdout/stderr
	// still go to /dev/null — that is the existing strategy-output policy.
	// /dev/null is bind-mounted into the rootfs before pivot; if it is
	// missing (minimal test roots) keep the inherited fds rather than fail exec.
	_ = discardStdio()
	err = unix.Exec(bin, ia.Argv, os.Environ())
	writeExecFailure(logFD, dupErr, bin, err)
	return 1, err
}

// writeExecFailure records an exec error on the CLOEXEC dup of the init log.
// discardStdio has already pointed fd 2 at /dev/null, so os.Stderr is useless.
func writeExecFailure(logFD int, dupErr error, bin string, err error) {
	if dupErr != nil || err == nil {
		return
	}
	f := os.NewFile(uintptr(logFD), OCIInitLogName)
	if f == nil {
		return
	}
	defer f.Close()
	fmt.Fprintf(f, "oci-init: exec %s: %v\n", bin, err)
}

func discardStdio() error {
	null, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer null.Close()
	fd := int(null.Fd())
	if err := unix.Dup2(fd, 1); err != nil {
		return err
	}
	return unix.Dup2(fd, 2)
}

func bindVolume(rootfs, hostPath, containerPath string) error {
	if hostPath == "" || containerPath == "" {
		return fmt.Errorf("empty volume bind")
	}
	abs, err := filepath.Abs(hostPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err != nil {
		return err
	}
	target, err := mkdirAllUnderRootfs(rootfs, containerPath)
	if err != nil {
		return err
	}
	return unix.Mount(abs, target, "", unix.MS_BIND, "")
}

func bindSame(rootfs, hostPath string, dir bool) error {
	if hostPath == "" {
		return nil
	}
	abs, err := filepath.Abs(hostPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err != nil {
		return err
	}
	target := filepath.Join(rootfs, abs)
	if dir {
		if err := os.MkdirAll(target, 0o755); err != nil {
			return err
		}
	} else {
		if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
			return err
		}
		if _, err := os.Stat(target); os.IsNotExist(err) {
			f, err := os.OpenFile(target, os.O_CREATE, 0o644)
			if err != nil {
				return err
			}
			f.Close()
		}
	}
	return unix.Mount(abs, target, "", unix.MS_BIND, "")
}

func bindConfig(rootfs, hostPath string) error {
	abs, err := filepath.Abs(hostPath)
	if err != nil {
		return err
	}
	if _, err := os.Stat(abs); err != nil {
		return err
	}
	// Host `current` is a symlink into releases/. Inside the rootfs it must
	// be a real directory so the bind target exists after pivot.
	target := filepath.Join(rootfs, abs)
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	if _, err := os.Stat(target); os.IsNotExist(err) {
		f, err := os.OpenFile(target, os.O_CREATE, 0o644)
		if err != nil {
			return err
		}
		f.Close()
	}
	return unix.Mount(abs, target, "", unix.MS_BIND, "")
}

func lookPathAfterPivot(cmd, pathEnv string) (string, error) {
	if strings.Contains(cmd, string(os.PathSeparator)) {
		if filepath.IsAbs(cmd) {
			return cmd, nil
		}
		abs, err := filepath.Abs(cmd)
		if err != nil {
			return "", err
		}
		return abs, nil
	}
	if pathEnv == "" {
		pathEnv = "/usr/local/bin:/usr/bin:/bin"
	}
	for _, dir := range strings.Split(pathEnv, ":") {
		if dir == "" {
			dir = "."
		}
		cand := filepath.Join(dir, cmd)
		if st, err := os.Stat(cand); err == nil && !st.IsDir() && st.Mode()&0o111 != 0 {
			return cand, nil
		}
	}
	return "", fmt.Errorf("command %q not found on PATH", cmd)
}
