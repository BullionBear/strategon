//go:build linux

package driver

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"time"

	"golang.org/x/sys/unix"
)

type stdioTeeOpts struct {
	LogDir  string
	Version string
	Argv    []string
	Env     []string
	Dir     string
}

func runStdioTeeFromArgs(args []string) int {
	opts := stdioTeeOpts{Env: os.Environ()}
	if wd, err := os.Getwd(); err == nil {
		opts.Dir = wd
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			opts.Argv = append([]string(nil), args[i+1:]...)
			break
		}
		key, val, ok := splitFlag(a)
		if !ok {
			fmt.Fprintln(os.Stderr, "stdio-tee: unexpected arg", a)
			return 2
		}
		switch key {
		case flagStdioLogDir:
			opts.LogDir = val
		case flagStdioVersion:
			opts.Version = val
		default:
			fmt.Fprintln(os.Stderr, "stdio-tee: unknown flag", key)
			return 2
		}
	}
	if opts.LogDir == "" || len(opts.Argv) == 0 {
		fmt.Fprintln(os.Stderr, "stdio-tee: --log-dir and argv are required")
		return 2
	}
	code, err := runStdioTee(opts)
	if err != nil {
		fmt.Fprintln(os.Stderr, "stdio-tee:", err)
		if code != 0 {
			return code
		}
		return 1
	}
	return code
}

func runStdioTee(opts stdioTeeOpts) (int, error) {
	rot, err := OpenSizeRotator(opts.LogDir, PayloadLogName, PayloadLogMaxBytes, PayloadLogArchives)
	if err != nil {
		return 1, err
	}
	defer rot.Close()

	pr, pw, err := os.Pipe()
	if err != nil {
		return 1, err
	}

	cmd := exec.Command(opts.Argv[0], opts.Argv[1:]...)
	cmd.Args = opts.Argv
	cmd.Env = opts.Env
	if cmd.Env == nil {
		cmd.Env = []string{}
	}
	cmd.Dir = opts.Dir
	cmd.Stdout = pw
	cmd.Stderr = pw

	if err := cmd.Start(); err != nil {
		pr.Close()
		pw.Close()
		return 1, err
	}
	childPID := cmd.Process.Pid
	_ = cmd.Process.Release()
	rot.SetMarker(StdioStartMarker(childPID, time.Now(), opts.Version))
	_ = pw.Close()

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, unix.SIGTERM, unix.SIGINT)
	defer signal.Stop(sigCh)
	go func() {
		for range sigCh {
			_ = unix.Kill(childPID, unix.SIGTERM)
		}
	}()

	copyDone := make(chan struct{})
	go func() {
		_, _ = io.Copy(rot, pr)
		close(copyDone)
	}()

	code := waitPayloadAndReap(childPID, os.Getpid() == 1)
	if err := pr.SetReadDeadline(time.Now().Add(stdioDrainAfter)); err != nil {
		_ = pr.Close()
	}
	<-copyDone
	_ = pr.Close()
	return code, nil
}

// waitPayloadAndReap waits for the payload pid. When we are pidns PID 1,
// other children are reaped so they do not zombie; those exits are ignored.
func waitPayloadAndReap(payloadPID int, pid1 bool) int {
	var ws unix.WaitStatus
	if !pid1 {
		for {
			wpid, err := unix.Wait4(payloadPID, &ws, 0, nil)
			if err == unix.EINTR {
				continue
			}
			if err != nil || wpid != payloadPID {
				return 0
			}
			return ws.ExitStatus()
		}
	}
	for {
		pid, err := unix.Wait4(-1, &ws, 0, nil)
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			if err == unix.ECHILD {
				return 0
			}
			return 1
		}
		if pid == payloadPID {
			return ws.ExitStatus()
		}
	}
}

func buildStdioTeeArgs(spec StartSpec) []string {
	args := []string{flagStdioTee, flagStdioLogDir + "=" + spec.PayloadLogDir}
	if spec.PayloadVersion != "" {
		args = append(args, flagStdioVersion+"="+spec.PayloadVersion)
	}
	args = append(args, "--", spec.BinaryPath)
	args = append(args, spec.Args...)
	return args
}
