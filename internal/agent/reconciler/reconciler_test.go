package reconciler

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/agent/artifact"
	"github.com/bullionbear/strategon/internal/agent/driver"
	"github.com/bullionbear/strategon/internal/agent/health"
	"github.com/bullionbear/strategon/internal/clock"
)

func artRef(version, digest string) *pb.ArtifactRef {
	return &pb.ArtifactRef{Type: pb.ArtifactType_ARTIFACT_TYPE_BINARY, Name: "strat", Version: version, Digest: digest}
}

func assignment(strategy, version, digest string, policy *pb.DeployPolicy) *pb.StrategyAssignmentSpec {
	return &pb.StrategyAssignmentSpec{Strategy: strategy, Artifact: artRef(version, digest), DeployPolicy: policy}
}

// newTestReconciler builds a reconciler wired to a fake driver, a real
// (temp-dir) artifact manager, a fake clock, and an always-ready health check.
// It does NOT start Run(); tests drive handlers synchronously for determinism.
func newTestReconciler(t *testing.T, start time.Time) (*Reconciler, *fakeDriver, *artifact.Manager, *clock.Fake, chan *pb.AgentMessage) {
	t.Helper()
	fd := newFakeDriver()
	t.Cleanup(fd.closeAll)
	base := t.TempDir()
	mgr := artifact.NewManager(base, artifact.LocalFetcher{})
	fk := clock.NewFake(start)
	out := make(chan *pb.AgentMessage, 128)
	r := New(Deps{
		Driver:    fd,
		Artifacts: mgr,
		Health:    health.AlwaysReady{},
		Clock:     fk,
		Out:       out,
		Jitter:    nil,
	})
	r.ctx = context.Background()
	return r, fd, mgr, fk, out
}

// seedRelease creates a release dir with a bin file and points current at it,
// so SwitchTo/rollback have real paths to operate on.
func seedRelease(t *testing.T, mgr *artifact.Manager, strategy, version string) {
	t.Helper()
	dir := mgr.ReleaseDir(strategy, version)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin"), []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileIdempotentSteadyState(t *testing.T) {
	r, fd, _, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	spec := assignment("s", "v1", "sha256:aaa", &pb.DeployPolicy{Startsecs: 5})
	r.generation = 7
	r.desired = map[string]*pb.StrategyAssignmentSpec{"s": spec}
	st := newStrategyState("s")
	st.phase = pb.DeployPhase_DEPLOY_PHASE_HEALTHY
	st.runningArtifact = artRef("v1", "sha256:aaa")
	st.proc = mustStart(t, fd)
	r.actual["s"] = st

	for i := 0; i < 5; i++ {
		r.reconcile()
	}
	if got := fd.starts(); got != 1 {
		t.Fatalf("steady state should not start new processes; starts=%d", got)
	}
	if st.observedGen != 7 {
		t.Fatalf("observedGen = %d, want 7", st.observedGen)
	}
}

func TestCrashBackoffThenRestart(t *testing.T) {
	t0 := time.Unix(1000, 0)
	r, fd, _, fk, _ := newTestReconciler(t, t0)
	policy := &pb.DeployPolicy{Startsecs: 5, MaxCrashesInWindow: 2}
	spec := assignment("s", "v1", "sha256:aaa", policy)
	r.desired = map[string]*pb.StrategyAssignmentSpec{"s": spec}
	st := newStrategyState("s")
	st.phase = pb.DeployPhase_DEPLOY_PHASE_HEALTHY
	st.runningArtifact = artRef("v1", "sha256:aaa")
	proc := mustStart(t, fd)
	proc.StartedAt = t0
	st.proc = proc
	r.actual["s"] = st
	startsBefore := fd.starts()

	// Process crashes after 1s (< startsecs) => counted as crash, backoff set.
	r.handleExit(processExit{strategy: "s", info: exitAt(proc, t0.Add(1*time.Second))})
	if st.backoff.Consecutive != 1 {
		t.Fatalf("consecutive = %d, want 1", st.backoff.Consecutive)
	}
	// reconcile now: backoff blocks restart.
	r.reconcile()
	if fd.starts() != startsBefore {
		t.Fatalf("restart should be held off by backoff")
	}
	// Advance past the 1s backoff; reconcile should restart.
	fk.Advance(2 * time.Second)
	r.reconcile()
	if fd.starts() != startsBefore+1 {
		t.Fatalf("expected restart after backoff elapsed; starts=%d", fd.starts())
	}
}

func TestPidReuseStaleExitIgnored(t *testing.T) {
	t0 := time.Unix(1000, 0)
	r, fd, _, _, _ := newTestReconciler(t, t0)
	spec := assignment("s", "v1", "sha256:aaa", &pb.DeployPolicy{Startsecs: 5})
	r.desired = map[string]*pb.StrategyAssignmentSpec{"s": spec}
	st := newStrategyState("s")
	st.phase = pb.DeployPhase_DEPLOY_PHASE_HEALTHY
	st.runningArtifact = artRef("v1", "sha256:aaa")
	proc := mustStart(t, fd)
	st.proc = proc
	r.actual["s"] = st

	// Stale exit: same pid, different startTime (pid reuse) => ignored.
	stale := exitAt(proc, t0.Add(time.Second))
	stale.StartTime = proc.StartTime + 999
	r.handleExit(processExit{strategy: "s", info: stale})
	if st.proc == nil {
		t.Fatalf("stale exit with mismatched startTime must be ignored")
	}
}

func TestDeployCancelledOnNewDesired(t *testing.T) {
	r, _, _, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	cancelled := false
	st := newStrategyState("s")
	st.inflight = &deployOp{target: artRef("v2", "sha256:bbb"), cancel: func() { cancelled = true }}
	r.actual["s"] = st

	// New desired points at a DIFFERENT digest => the in-flight deploy is cancelled.
	ds := &pb.DesiredState{
		Generation:  10,
		Assignments: []*pb.StrategyAssignmentSpec{assignment("s", "v3", "sha256:ccc", &pb.DeployPolicy{})},
	}
	r.applyDesired(ds)
	if !cancelled {
		t.Fatalf("in-flight deploy to v2 should be cancelled when desired changes to v3")
	}
	if st.inflight != nil {
		t.Fatalf("inflight should be cleared after cancellation")
	}
}

func TestDeployNotCancelledWhenTargetUnchanged(t *testing.T) {
	r, _, _, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	cancelled := false
	st := newStrategyState("s")
	st.inflight = &deployOp{target: artRef("v2", "sha256:bbb"), cancel: func() { cancelled = true }}
	r.actual["s"] = st

	ds := &pb.DesiredState{
		Generation:  10,
		Assignments: []*pb.StrategyAssignmentSpec{assignment("s", "v2", "sha256:bbb", &pb.DeployPolicy{})},
	}
	r.applyDesired(ds)
	if cancelled {
		t.Fatalf("deploy to the same target must not be cancelled")
	}
	if st.inflight == nil {
		t.Fatalf("inflight to same target should remain")
	}
}

func TestAutoRollbackOnCrashLoopAndSkipBadVersion(t *testing.T) {
	t0 := time.Unix(1000, 0)
	r, fd, mgr, _, out := newTestReconciler(t, t0)
	seedRelease(t, mgr, "s", "v1")
	seedRelease(t, mgr, "s", "v2")

	policy := &pb.DeployPolicy{Startsecs: 5, MaxCrashesInWindow: 2, EnableAutoRollback: true, HealthWindowSeconds: 120}
	spec := assignment("s", "v2", "sha256:v2", policy)
	r.desired = map[string]*pb.StrategyAssignmentSpec{"s": spec}

	st := newStrategyState("s")
	st.phase = pb.DeployPhase_DEPLOY_PHASE_HEALTH_CHECKING
	st.inflight = &deployOp{target: artRef("v2", "sha256:v2"), cancel: func() {}}
	st.runningArtifact = artRef("v2", "sha256:v2")
	st.prevArtifact = artRef("v1", "sha256:v1")
	r.actual["s"] = st

	// Three fast crashes in the health window (> maxCrashes=2) => auto rollback.
	for i := 0; i < 3; i++ {
		proc := mustStart(t, fd)
		proc.StartedAt = t0
		st.proc = proc
		r.handleExit(processExit{strategy: "s", info: exitAt(proc, t0.Add(time.Second))})
	}

	if st.lastBadVersion != "v2" {
		t.Fatalf("lastBadVersion = %q, want v2", st.lastBadVersion)
	}
	if st.phase != pb.DeployPhase_DEPLOY_PHASE_ROLLED_BACK {
		t.Fatalf("phase = %v, want ROLLED_BACK", st.phase)
	}
	if st.runningArtifact.GetVersion() != "v1" {
		t.Fatalf("running version = %q, want v1", st.runningArtifact.GetVersion())
	}
	if mgr.CurrentVersion("s") != "v1" {
		t.Fatalf("current symlink = %q, want v1", mgr.CurrentVersion("s"))
	}

	// reconcile: desired still v2 (bad) => must be skipped, not redeployed.
	st.inflight = nil
	drainEvents(out)
	r.reconcile()
	if st.inflight != nil {
		t.Fatalf("known-bad version v2 must not be redeployed")
	}
	if !sawEvent(out, "SkipBadVersion") {
		t.Fatalf("expected SkipBadVersion event")
	}
}

// --- helpers ---

func mustStart(t *testing.T, fd *fakeDriver) *driver.Process {
	t.Helper()
	p, err := fd.Start(driver.StartSpec{Strategy: "s"}, time.Unix(1000, 0))
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func exitAt(p *driver.Process, at time.Time) driver.ExitInfo {
	return driver.ExitInfo{PID: p.PID, StartTime: p.StartTime, At: at}
}

func drainEvents(out chan *pb.AgentMessage) {
	for {
		select {
		case <-out:
		default:
			return
		}
	}
}

func sawEvent(out chan *pb.AgentMessage, reason string) bool {
	for {
		select {
		case m := <-out:
			if ev := m.GetEvent(); ev != nil && ev.GetReason() == reason {
				return true
			}
		default:
			return false
		}
	}
}
