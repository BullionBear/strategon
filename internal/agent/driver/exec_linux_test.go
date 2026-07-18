//go:build linux

package driver

import (
	"os/exec"
	"syscall"
	"testing"
	"time"
)

func TestExecDriverStartSignalWatch(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available")
	}
	d := NewExecDriver("") // no cgroup confinement
	p, err := d.Start(StartSpec{Strategy: "s", BinaryPath: sleep, Args: []string{"30"}}, time.Now())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if p.PID <= 0 {
		t.Fatalf("bad pid %d", p.PID)
	}
	if p.StartTime == 0 {
		t.Fatalf("expected non-zero starttime")
	}
	if !processAlive(p.PID, p.StartTime) {
		t.Fatalf("process should be alive")
	}

	// A stale (pid, wrong-starttime) pair must be treated as not-alive
	// (pid-reuse guard).
	if processAlive(p.PID, p.StartTime+1) {
		t.Fatalf("starttime mismatch should read as not-alive")
	}

	exited := make(chan ExitInfo, 1)
	go func() { exited <- d.WatchExit(p, time.Now) }()

	if err := d.Signal(p, syscall.SIGKILL); err != nil {
		t.Fatalf("signal: %v", err)
	}
	select {
	case <-exited:
	case <-time.After(5 * time.Second):
		t.Fatalf("WatchExit did not return after kill")
	}
}

func TestExecDriverAdoptRejectsPidReuse(t *testing.T) {
	sleep, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("sleep not available")
	}
	d := NewExecDriver("")
	p, err := d.Start(StartSpec{Strategy: "s", BinaryPath: sleep, Args: []string{"30"}}, time.Now())
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	defer d.Signal(p, syscall.SIGKILL)

	if _, err := d.Adopt(p.PID, p.StartTime, time.Now()); err != nil {
		t.Fatalf("adopt with correct starttime should succeed: %v", err)
	}
	if _, err := d.Adopt(p.PID, p.StartTime+1, time.Now()); err == nil {
		t.Fatalf("adopt with wrong starttime should fail (pid reuse)")
	}
}
