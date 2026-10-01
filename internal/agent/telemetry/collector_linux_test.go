//go:build linux

package telemetry

import (
	"context"
	"os"
	"testing"
	"time"

	"github.com/bullionbear/strategon/internal/agent/driver"
)

// MachineSpecFromHost probes for user namespaces and host-PID OCI, and each
// probe re-execs this binary (--oci-probe / --oci-probe-host-pid). Answer that
// here or the re-exec would try to run the test suite again and the probe
// would report a false negative.
func TestMain(m *testing.M) {
	if driver.MaybeRunOCIHelper() {
		return
	}
	os.Exit(m.Run())
}

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
	if spec.GetOciHostPidAvailable() != driver.HostPIDAvailable() {
		t.Fatalf("oci_host_pid_available=%v, probe=%v", spec.GetOciHostPidAvailable(), driver.HostPIDAvailable())
	}
}

// Slot accounting is read for dead targets too (descendants outlive the
// payload), the first sample only sets the OOM baseline, and a rise in
// oom_kill reports the delta once.
func TestCollectorSlotCgroupOOM(t *testing.T) {
	root := t.TempDir()
	slot := root + "/s"
	if err := os.MkdirAll(slot, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name, body string) {
		if err := os.WriteFile(slot+"/"+name, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	write("memory.current", "1048576\n")
	write("memory.peak", "2097152\n")
	write("memory.events", "low 0\nhigh 0\nmax 4\noom 2\noom_kill 2\noom_group_kill 0\n")

	type call struct{ total, delta int64 }
	var calls []call
	c := New(func() []ProcessTarget { return []ProcessTarget{{Strategy: "s"}} })
	c.CgroupRoot = root
	c.OnOOMKill = func(_ string, total, delta int64) { calls = append(calls, call{total, delta}) }

	c.sample()
	pm := c.Latest().Processes[0]
	if pm.GetMemoryCurrentBytes() != 1<<20 || pm.GetMemoryPeakBytes() != 2<<20 || pm.GetOomKills() != 2 {
		t.Fatalf("slot stats = %d/%d/%d", pm.GetMemoryCurrentBytes(), pm.GetMemoryPeakBytes(), pm.GetOomKills())
	}
	if len(calls) != 0 {
		t.Fatalf("first sample must only set the baseline, got %v", calls)
	}

	write("memory.events", "oom_kill 5\n")
	c.sample()
	c.sample()
	if len(calls) != 1 || calls[0] != (call{5, 3}) {
		t.Fatalf("want one OOM report {5 3}, got %v", calls)
	}
}
