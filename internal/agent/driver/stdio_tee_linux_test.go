//go:build linux

package driver

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStdioTeeCapturesAndMarks(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	code, err := runStdioTee(stdioTeeOpts{
		LogDir:  dir,
		Version: "v9",
		Argv:    []string{sh, "-c", "printf 'hello-tee\\n'"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	body, err := os.ReadFile(filepath.Join(dir, PayloadLogName))
	if err != nil {
		t.Fatal(err)
	}
	s := string(body)
	if !strings.Contains(s, "hello-tee") {
		t.Fatalf("log = %q", s)
	}
	if !strings.Contains(s, "=== start pid=") || !strings.Contains(s, "version=v9") {
		t.Fatalf("missing marker: %q", s)
	}
}

func TestStdioTeeExitsWhenChildExitsDespiteInheritedStdout(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	done := make(chan struct{})
	go func() {
		_, _ = runStdioTee(stdioTeeOpts{
			LogDir: dir,
			Argv:   []string{sh, "-c", "sleep 30 & printf done\\n; exit 0"},
		})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("tee must exit on wait4(child), not pipe EOF")
	}
}

func TestStdioTeeMergesStdoutStderrWithoutTear(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	script := `i=0
while [ "$i" -lt 80 ]; do
  printf 'OUT-%02d\n' "$i"
  i=$((i+1))
done &
j=0
while [ "$j" -lt 80 ]; do
  printf 'ERR-%02d\n' "$j" >&2
  j=$((j+1))
done &
wait
`
	code, err := runStdioTee(stdioTeeOpts{
		LogDir: dir,
		Argv:   []string{sh, "-c", script},
	})
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit %d", code)
	}
	body, err := os.ReadFile(filepath.Join(dir, PayloadLogName))
	if err != nil {
		t.Fatal(err)
	}
	for _, line := range strings.Split(string(body), "\n") {
		if line == "" || strings.HasPrefix(line, "===") {
			continue
		}
		if !strings.HasPrefix(line, "OUT-") && !strings.HasPrefix(line, "ERR-") {
			t.Fatalf("torn line %q in %q", line, body)
		}
	}
}

func TestStdioTeeSIGTERMWaitsForChild(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	cmd := exec.Command(os.Args[0], flagStdioTee, flagStdioLogDir+"="+dir, "--", sh, "-c",
		`trap 'printf got-term\n; sleep 0.25; exit 0' TERM; printf started\n; sleep 30`)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(dir, PayloadLogName)
	deadline := time.Now().Add(3 * time.Second)
	for {
		body, _ := os.ReadFile(logPath)
		if strings.Contains(string(body), "started") {
			break
		}
		if time.Now().After(deadline) {
			_ = cmd.Process.Kill()
			t.Fatal("child did not start")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := syscall.Kill(-cmd.Process.Pid, syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	time.Sleep(80 * time.Millisecond)
	if err := cmd.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatal("tee exited before the child finished its SIGTERM handler")
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("wait: %v", err)
		}
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		t.Fatal("tee did not exit after the child")
	}
	body, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "got-term") {
		t.Fatalf("log = %q", body)
	}
}

func TestStdioTeeWriteErrorDoesNotBlockPayload(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip(err)
	}
	dir := t.TempDir()
	big := filepath.Join(t.TempDir(), "big")
	if err := os.WriteFile(big, bytes.Repeat([]byte("x"), 12<<20), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() {
		_, err := runStdioTee(stdioTeeOpts{
			LogDir: dir,
			Argv:   []string{sh, "-c", "cat " + big},
		})
		done <- err
	}()
	logPath := filepath.Join(dir, PayloadLogName)
	deadline := time.Now().Add(2 * time.Second)
	for {
		if _, err := os.Stat(logPath); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("payload.log never appeared")
		}
		time.Sleep(5 * time.Millisecond)
	}
	if err := os.Chmod(dir, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o755) })
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("payload blocked after rotator write error")
	}
}

func TestExecDriverCaptureStdio(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip(err)
	}
	work := t.TempDir()
	logDir := filepath.Join(work, StdioDirName)
	d := NewExecDriver("")
	p, err := d.Start(StartSpec{
		Strategy:       "s",
		BinaryPath:     sh,
		Args:           []string{"-c", "printf 'from-exec\\n'"},
		WorkDir:        work,
		CaptureStdio:   true,
		PayloadLogDir:  logDir,
		PayloadVersion: "v1",
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	info := d.WatchExit(p, time.Now)
	if info.Code != 0 {
		t.Fatalf("exit %d", info.Code)
	}
	body, err := os.ReadFile(filepath.Join(logDir, PayloadLogName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "from-exec") {
		t.Fatalf("log = %q", body)
	}
}

func TestExecDriverCaptureOffDiscards(t *testing.T) {
	sh, err := exec.LookPath("sh")
	if err != nil {
		t.Skip(err)
	}
	work := t.TempDir()
	logDir := filepath.Join(work, StdioDirName)
	if err := os.MkdirAll(logDir, 0o750); err != nil {
		t.Fatal(err)
	}
	d := NewExecDriver("")
	p, err := d.Start(StartSpec{
		Strategy:      "s",
		BinaryPath:    sh,
		Args:          []string{"-c", "printf 'should-discard\\n'"},
		WorkDir:       work,
		PayloadLogDir: logDir,
	}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if info := d.WatchExit(p, time.Now); info.Code != 0 {
		t.Fatalf("exit %d", info.Code)
	}
	if _, err := os.Stat(filepath.Join(logDir, PayloadLogName)); !os.IsNotExist(err) {
		t.Fatalf("capture off must not write payload.log: %v", err)
	}
}
