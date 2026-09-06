//go:build linux

package driver

import (
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestMain(m *testing.M) {
	if MaybeRunOCIHelper() {
		return
	}
	os.Exit(m.Run())
}

// requireUserNS skips locally (a dev box may block user namespaces) but fails
// where STRATEGON_REQUIRE_USERNS is set, so CI cannot go green by skipping the
// only tests that exercise --oci-init.
func requireUserNS(t *testing.T) {
	t.Helper()
	if UserNSAvailable() {
		return
	}
	if os.Getenv("STRATEGON_REQUIRE_USERNS") != "" {
		t.Fatal("unprivileged user namespaces unavailable but STRATEGON_REQUIRE_USERNS is set")
	}
	t.Skip("unprivileged user namespaces unavailable")
}

func TestUserNSAvailableOrSkip(t *testing.T) {
	requireUserNS(t)
}

func TestOCIDriverStartSignalWatch(t *testing.T) {
	requireUserNS(t)
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available")
	}
	rootfs := t.TempDir()
	binDir := filepath.Join(rootfs, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(sleep)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(binDir, "sleep"), data, 0o755); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(work, 0o755); err != nil {
		t.Fatal(err)
	}

	d := NewOCIDriver(NewExecDriver(""))
	p, err := d.Start(StartSpec{
		Strategy: "s",
		Driver:   KindOCI,
		Rootfs:   rootfs,
		Argv:     []string{"sleep", "30"},
		WorkDir:  work,
		WorkBind: work,
		Env:      []string{"PATH=/bin"},
	}, time.Now())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if p.PID <= 0 {
		t.Fatalf("bad pid %d", p.PID)
	}

	exited := make(chan ExitInfo, 1)
	go func() { exited <- d.WatchExit(p, time.Now) }()
	if err := d.Signal(p, syscall.SIGKILL); err != nil {
		t.Fatalf("signal: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatal("WatchExit did not return")
	}
}

func TestOCIDriverRejectsEmptyArgv(t *testing.T) {
	d := NewOCIDriver(NewExecDriver(""))
	if _, err := d.Start(StartSpec{Driver: KindOCI, Rootfs: t.TempDir()}, time.Now()); err == nil {
		t.Fatal("expected error")
	}
}
