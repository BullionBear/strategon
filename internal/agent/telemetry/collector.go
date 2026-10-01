// Package telemetry samples host and per-strategy process resources from /proc
// for heartbeats and the optional /metrics scrape endpoint. Sampling runs in a
// dedicated goroutine so /proc IO never blocks the reconciler loop.
package telemetry

import (
	"context"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/agent/driver"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// ProcessTarget is a read-only snapshot of a managed process published by the
// reconciler. The collector must not touch reconciler state directly.
type ProcessTarget struct {
	Strategy     string
	PID          int32
	Alive        bool
	RestartCount int32
}

// TargetsFunc returns the current set of managed processes (safe for concurrent calls).
type TargetsFunc func() []ProcessTarget

// Collector periodically samples machine and process resources.
type Collector struct {
	Interval time.Duration
	Targets  TargetsFunc
	Logger   *slog.Logger

	// CgroupRoot is the agent's prepared slot root; empty skips cgroup
	// accounting. OnOOMKill, if set, is called from the sampling goroutine
	// when a slot's oom_kill counter rises between samples.
	CgroupRoot string
	OnOOMKill  func(strategy string, total, delta int64)

	mu       sync.Mutex
	prevCPU  *hostCPUSample
	prevProc map[string]procCPUSample // strategy -> last cpu sample
	prevOOM  map[string]int64         // slot -> last oom_kill count
	oomBased bool                     // prevOOM seeded from every existing slot

	snapshot atomic.Pointer[Snapshot]
}

// Snapshot is the latest sample, safe to read concurrently.
type Snapshot struct {
	Resources *pb.MachineResources
	Processes []*pb.ProcessMetrics
	At        time.Time
}

// New returns a Collector with a sensible default interval.
func New(targets TargetsFunc) *Collector {
	return &Collector{
		Interval: 10 * time.Second,
		Targets:  targets,
		prevProc: map[string]procCPUSample{},
		prevOOM:  map[string]int64{},
	}
}

// Run samples until ctx is cancelled.
func (c *Collector) Run(ctx context.Context) {
	interval := c.Interval
	if interval <= 0 {
		interval = 10 * time.Second
	}
	// First sample establishes CPU baselines (percent may be 0).
	c.sample()
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			c.sample()
		}
	}
}

// Latest returns a proto clone of the most recent snapshot (may be nil).
func (c *Collector) Latest() *Snapshot {
	s := c.snapshot.Load()
	if s == nil {
		return nil
	}
	out := &Snapshot{At: s.At}
	if s.Resources != nil {
		out.Resources = proto.Clone(s.Resources).(*pb.MachineResources)
	}
	if len(s.Processes) > 0 {
		out.Processes = make([]*pb.ProcessMetrics, len(s.Processes))
		for i, p := range s.Processes {
			out.Processes[i] = proto.Clone(p).(*pb.ProcessMetrics)
		}
	}
	return out
}

// HeartbeatResources returns the latest MachineResources for a Heartbeat.
func (c *Collector) HeartbeatResources() *pb.MachineResources {
	s := c.Latest()
	if s == nil {
		return nil
	}
	return s.Resources
}

// HeartbeatProcesses returns the latest ProcessMetrics for a Heartbeat.
func (c *Collector) HeartbeatProcesses() []*pb.ProcessMetrics {
	s := c.Latest()
	if s == nil {
		return nil
	}
	return s.Processes
}

func (c *Collector) logger() *slog.Logger {
	if c.Logger != nil {
		return c.Logger
	}
	return slog.Default()
}

func (c *Collector) sample() {
	now := time.Now().UTC()
	res, hostCPU, err := sampleMachine(c.prevCPU)
	if err != nil {
		c.logger().Debug("machine sample failed", "err", err)
	} else if res != nil {
		res.CollectedAt = timestamppb.New(now)
	}

	var targets []ProcessTarget
	if c.Targets != nil {
		targets = c.Targets()
	}

	c.mu.Lock()
	defer c.mu.Unlock()
	if hostCPU != nil {
		c.prevCPU = hostCPU
	}

	procs := make([]*pb.ProcessMetrics, 0, len(targets))
	seen := make(map[string]struct{}, len(targets))
	for _, t := range targets {
		seen[t.Strategy] = struct{}{}
		pm := &pb.ProcessMetrics{
			Strategy:     t.Strategy,
			Pid:          t.PID,
			Alive:        t.Alive,
			RestartCount: t.RestartCount,
		}
		if t.Alive && t.PID > 0 {
			prev := c.prevProc[t.Strategy]
			rss, fds, cpu, next, perr := sampleProcess(t.PID, prev)
			if perr != nil {
				c.logger().Debug("process sample failed", "strategy", t.Strategy, "pid", t.PID, "err", perr)
			} else {
				pm.RssBytes = rss
				pm.NumFds = fds
				pm.CpuPercent = cpu
				c.prevProc[t.Strategy] = next
			}
		}
		procs = append(procs, pm)
	}
	for strat := range c.prevProc {
		if _, ok := seen[strat]; !ok {
			delete(c.prevProc, strat)
		}
	}
	c.seedOOMBaseline()
	for _, pm := range procs {
		c.sampleCgroup(pm)
	}

	c.snapshot.Store(&Snapshot{
		Resources: res,
		Processes: procs,
		At:        now,
	})
}

// seedOOMBaseline records oom_kill for every slot cgroup that exists when the
// collector first runs, whether or not the reconciler has published it as a
// target yet. Those counts may predate this agent (adopted payloads) and were
// already reported. Any slot first seen later starts from 0, so an OOM in its
// first sampling interval is reported too.
func (c *Collector) seedOOMBaseline() {
	if c.oomBased || c.CgroupRoot == "" {
		return
	}
	c.oomBased = true
	entries, err := os.ReadDir(c.CgroupRoot)
	if err != nil {
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, _, kills, ok := driver.SlotCgroupStats(c.CgroupRoot, e.Name()); ok {
			c.prevOOM[e.Name()] = kills
		}
	}
}

// sampleCgroup fills slot accounting. The slot outlives its payload (setsid
// descendants stay in it), so it is read whether or not the target is alive.
// prevOOM is kept for slots that stop being targets: the cgroup and its
// counter survive, and a re-added slot must not report old kills again.
func (c *Collector) sampleCgroup(pm *pb.ProcessMetrics) {
	cur, peak, kills, ok := driver.SlotCgroupStats(c.CgroupRoot, pm.GetStrategy())
	if !ok {
		return
	}
	pm.MemoryCurrentBytes = cur
	pm.MemoryPeakBytes = peak
	pm.OomKills = kills
	prev := c.prevOOM[pm.GetStrategy()]
	c.prevOOM[pm.GetStrategy()] = kills
	if kills > prev && c.OnOOMKill != nil {
		c.OnOOMKill(pm.GetStrategy(), kills, kills-prev)
	}
}
