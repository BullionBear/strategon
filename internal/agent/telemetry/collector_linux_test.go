//go:build linux

package telemetry

import (
	"context"
	"testing"
	"time"
)

func TestSampleMachine(t *testing.T) {
	res, cur, err := sampleMachine(nil)
	if err != nil {
		t.Fatal(err)
	}
	if res == nil || cur == nil {
		t.Fatal("expected machine sample")
	}
	if res.GetMemoryTotalBytes() <= 0 {
		t.Fatalf("memory total=%d", res.GetMemoryTotalBytes())
	}
	// Second sample should yield a cpu percent (may be ~0 on idle CI).
	res2, _, err := sampleMachine(cur)
	if err != nil {
		t.Fatal(err)
	}
	if res2.GetCpuPercent() < 0 || res2.GetCpuPercent() > 100 {
		t.Fatalf("cpu percent out of range: %v", res2.GetCpuPercent())
	}
}

func TestCollectorHeartbeat(t *testing.T) {
	c := New(func() []ProcessTarget {
		return []ProcessTarget{{Strategy: "self", PID: int32(1), Alive: true}}
	})
	c.Interval = 20 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go c.Run(ctx)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if s := c.Latest(); s != nil && s.Resources != nil && len(s.Processes) == 1 {
			cancel()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("collector never produced a snapshot")
}

func TestMachineSpecFromHost(t *testing.T) {
	spec := MachineSpecFromHost()
	if spec.GetNumCpus() <= 0 {
		t.Fatalf("num_cpus=%d", spec.GetNumCpus())
	}
	if spec.GetOs() != "linux" {
		t.Fatalf("os=%q", spec.GetOs())
	}
}
