//go:build linux

package driver

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"syscall"

	"golang.org/x/sys/unix"
)

// checkRlimitNofile rejects a max_open_files the payload could not be given.
// A non-root helper can lower the hard limit but not raise it, so anything
// above the agent's own hard limit would fail inside the child, where the
// error has nowhere visible to go.
func checkRlimitNofile(n int32) error {
	if n <= 0 {
		return nil
	}
	var lim unix.Rlimit
	if err := unix.Getrlimit(unix.RLIMIT_NOFILE, &lim); err != nil {
		return fmt.Errorf("max_open_files: %w", err)
	}
	if uint64(n) > lim.Max {
		return fmt.Errorf("max_open_files %d exceeds the agent's hard limit %d (raise LimitNOFILE= on the agent unit)", n, lim.Max)
	}
	return nil
}

// applyRlimitNofile sets soft and hard RLIMIT_NOFILE to the flag value, so
// the payload can neither exceed nor raise it.
func applyRlimitNofile(val string) error {
	n, err := strconv.ParseUint(val, 10, 64)
	if err != nil || n == 0 {
		return fmt.Errorf("%s=%s: invalid", flagRlimitNofile, val)
	}
	return unix.Setrlimit(unix.RLIMIT_NOFILE, &unix.Rlimit{Cur: n, Max: n})
}

// runExecPayload replaces this process with argv[0] (after "--"). The pid and
// /proc starttime the agent recorded at Start stay valid across exec.
func runExecPayload(args []string) int {
	if len(args) < 2 || args[0] != "--" {
		fmt.Fprintln(os.Stderr, "exec-payload: usage: --exec-payload -- argv...")
		return 2
	}
	argv := args[1:]
	path := argv[0]
	if lp, err := exec.LookPath(path); err == nil {
		path = lp
	}
	err := syscall.Exec(path, argv, os.Environ())
	fmt.Fprintln(os.Stderr, "exec-payload:", err)
	if errors.Is(err, syscall.ENOENT) {
		return 127
	}
	return 126
}
