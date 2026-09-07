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
	if err := applyRootfs(ia); err != nil {
		fmt.Fprintln(os.Stderr, "oci-init:", err)
		return 1
	}
	return 1
}

func applyRootfs(ia InitArgs) error {
	if ia.Rootfs == "" {
		return fmt.Errorf("missing --oci-rootfs")
	}
	rootfs, err := filepath.Abs(ia.Rootfs)
	if err != nil {
		return err
	}

	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_PRIVATE, ""); err != nil {
		return fmt.Errorf("make-rprivate: %w", err)
	}
	if err := unix.Mount(rootfs, rootfs, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind rootfs: %w", err)
	}
	oldroot := filepath.Join(rootfs, ".oldroot")
	if err := os.MkdirAll(oldroot, 0o700); err != nil {
		return fmt.Errorf("mkdir .oldroot: %w", err)
	}

	if err := bindSame(rootfs, ia.Work, true); err != nil {
		return fmt.Errorf("bind work: %w", err)
	}
	if err := bindSame(rootfs, ia.Shared, true); err != nil {
		return fmt.Errorf("bind shared: %w", err)
	}
	if ia.Config != "" {
		if err := bindConfig(rootfs, ia.Config); err != nil {
			return fmt.Errorf("bind config: %w", err)
		}
	}
	for _, p := range []string{"/etc/resolv.conf", "/dev/null", "/dev/zero", "/dev/urandom"} {
		if err := bindSame(rootfs, p, false); err != nil {
			// resolv.conf / devices may be missing in some test roots; skip if host path absent.
			if os.IsNotExist(err) {
				continue
			}
			return fmt.Errorf("bind %s: %w", p, err)
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
		return err
	}
	if err := unix.Mount("proc", procTarget, "proc",
		unix.MS_NOSUID|unix.MS_NODEV|unix.MS_NOEXEC, ""); err != nil {
		return fmt.Errorf("mount proc: %w", err)
	}

	if err := unix.PivotRoot(rootfs, oldroot); err != nil {
		return fmt.Errorf("pivot_root: %w", err)
	}
	if err := os.Chdir("/"); err != nil {
		return err
	}
	if err := unix.Unmount("/.oldroot", unix.MNT_DETACH); err != nil {
		return fmt.Errorf("unmount oldroot: %w", err)
	}
	_ = os.Remove("/.oldroot")

	cwd := ia.CWD
	if cwd == "" {
		cwd = ia.Work
	}
	if cwd != "" {
		if err := os.MkdirAll(cwd, 0o755); err != nil {
			return fmt.Errorf("mkdir cwd: %w", err)
		}
		if err := os.Chdir(cwd); err != nil {
			return fmt.Errorf("chdir %s: %w", cwd, err)
		}
	}

	if len(ia.Argv) == 0 {
		return fmt.Errorf("empty argv")
	}
	bin, err := lookPathAfterPivot(ia.Argv[0], os.Getenv("PATH"))
	if err != nil {
		return err
	}
	return unix.Exec(bin, ia.Argv, os.Environ())
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
