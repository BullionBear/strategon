package reconciler

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/agent/artifact"
	"github.com/bullionbear/strategon/internal/agent/driver"
	"github.com/bullionbear/strategon/internal/agent/health"
	"github.com/bullionbear/strategon/internal/clock"
	"google.golang.org/protobuf/proto"
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

type countFetcher struct{ n int }

func (c *countFetcher) Fetch(context.Context, *pb.ArtifactRef, string) error {
	c.n++
	return fmt.Errorf("fetch must not run on same-digest retag")
}

func TestSameDigestPromotesVersionAndURI(t *testing.T) {
	r, fd, mgr, _, out := newTestReconciler(t, time.Unix(1000, 0))
	cf := &countFetcher{}
	mgr.Fetcher = cf

	running := &pb.ArtifactRef{
		Type: pb.ArtifactType_ARTIFACT_TYPE_OCI_IMAGE, Name: "mftik-sym",
		Version: "v2", Digest: "sha256:aaa", Uri: "http://100.65.26.119:8899/mftik-sym.tar",
	}
	desired := proto.Clone(running).(*pb.ArtifactRef)
	desired.Version = "v3"
	desired.Uri = "s3://strategon-artifacts/artifacts/mftik-sym/v3/aaa"

	spec := &pb.StrategyAssignmentSpec{Strategy: "s", Artifact: desired, DeployPolicy: &pb.DeployPolicy{Startsecs: 5}}
	r.generation = 20
	r.desired = map[string]*pb.StrategyAssignmentSpec{"s": spec}
	st := newStrategyState("s")
	st.phase = pb.DeployPhase_DEPLOY_PHASE_HEALTHY
	st.runningArtifact = running
	st.proc = mustStart(t, fd)
	r.actual["s"] = st
	starts := fd.starts()

	r.reconcile()
	if cf.n != 0 {
		t.Fatalf("same-digest retag must not fetch; calls=%d", cf.n)
	}
	if st.inflight != nil {
		t.Fatal("same-digest retag must not beginDeploy")
	}
	if st.phase != pb.DeployPhase_DEPLOY_PHASE_HEALTHY {
		t.Fatalf("phase = %s, want HEALTHY", st.phase)
	}
	if fd.starts() != starts {
		t.Fatalf("must not restart the process; starts=%d", fd.starts())
	}
	if st.runningArtifact.GetVersion() != "v3" || st.runningArtifact.GetUri() != desired.GetUri() {
		t.Fatalf("running = %+v, want v3 / s3 uri", st.runningArtifact)
	}
	if st.observedGen != 20 {
		t.Fatalf("observedGen = %d, want 20", st.observedGen)
	}
	if !sawEvent(out, "VersionRelabeled") {
		t.Fatal("expected VersionRelabeled event")
	}

	drainEvents(out)
	r.reconcile()
	if sawEvent(out, "VersionRelabeled") {
		t.Fatal("second tick must not emit VersionRelabeled again")
	}
	if cf.n != 0 || st.inflight != nil {
		t.Fatal("second tick must stay idle")
	}
}

func TestDifferentDigestStillBeginsDeploy(t *testing.T) {
	r, fd, _, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	spec := assignment("s", "v3", "sha256:bbb", &pb.DeployPolicy{Startsecs: 5})
	spec.Artifact.Uri = "s3://bucket/v3"
	r.generation = 3
	r.desired = map[string]*pb.StrategyAssignmentSpec{"s": spec}
	st := newStrategyState("s")
	st.phase = pb.DeployPhase_DEPLOY_PHASE_HEALTHY
	st.runningArtifact = &pb.ArtifactRef{
		Type: pb.ArtifactType_ARTIFACT_TYPE_BINARY, Name: "strat",
		Version: "v2", Digest: "sha256:aaa", Uri: "http://old/v2",
	}
	st.proc = mustStart(t, fd)
	r.actual["s"] = st

	r.reconcile()
	if st.inflight == nil {
		t.Fatal("different digest must beginDeploy")
	}
	if st.runningArtifact.GetVersion() != "v2" {
		t.Fatalf("running version should stay v2 until deploy finishes, got %q", st.runningArtifact.GetVersion())
	}
	waitWorkerPhase(t, r, pb.DeployPhase_DEPLOY_PHASE_FAILED)
}

func TestSameDigestConfigLabelPromotes(t *testing.T) {
	r, fd, mgr, _, out := newTestReconciler(t, time.Unix(1000, 0))
	cf := &countFetcher{}
	mgr.Fetcher = cf

	art := artRef("v1", "sha256:aaa")
	art.Uri = "s3://bucket/bin"
	oldCfg := &pb.ArtifactRef{Name: "s-config", Version: "c1", Digest: "sha256:cfg", Uri: "file:///tmp/c1.yml"}
	newCfg := &pb.ArtifactRef{Name: "s-config", Version: "c2", Digest: "sha256:cfg", Uri: "s3://bucket/c2.yml"}

	spec := &pb.StrategyAssignmentSpec{
		Strategy: "s", Artifact: art, Config: newCfg,
		DeployPolicy: &pb.DeployPolicy{Startsecs: 5},
	}
	r.generation = 9
	r.desired = map[string]*pb.StrategyAssignmentSpec{"s": spec}
	st := newStrategyState("s")
	st.phase = pb.DeployPhase_DEPLOY_PHASE_HEALTHY
	st.runningArtifact = proto.Clone(art).(*pb.ArtifactRef)
	st.runningConfig = oldCfg
	st.proc = mustStart(t, fd)
	r.actual["s"] = st

	r.reconcile()
	if cf.n != 0 || st.inflight != nil {
		t.Fatal("config-only label change must not fetch or deploy")
	}
	if st.runningConfig.GetVersion() != "c2" || st.runningConfig.GetUri() != newCfg.GetUri() {
		t.Fatalf("runningConfig = %+v", st.runningConfig)
	}
	if st.runningArtifact.GetVersion() != "v1" {
		t.Fatalf("artifact version should stay v1, got %q", st.runningArtifact.GetVersion())
	}
	if st.observedGen != 9 {
		t.Fatalf("observedGen = %d, want 9", st.observedGen)
	}
	if !sawEvent(out, "VersionRelabeled") {
		t.Fatal("expected VersionRelabeled for config retag")
	}
}

func TestCrashRestartPromotesLabels(t *testing.T) {
	r, fd, mgr, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	cf := &countFetcher{}
	mgr.Fetcher = cf
	seedRelease(t, mgr, "s", "v2")
	if err := mgr.SwitchTo("s", "v2"); err != nil {
		t.Fatal(err)
	}

	desired := &pb.ArtifactRef{
		Type: pb.ArtifactType_ARTIFACT_TYPE_BINARY, Name: "strat",
		Version: "v3", Digest: "sha256:aaa", Uri: "s3://bucket/v3",
	}
	spec := &pb.StrategyAssignmentSpec{Strategy: "s", Artifact: desired, DeployPolicy: &pb.DeployPolicy{Startsecs: 5}}
	r.generation = 4
	r.desired = map[string]*pb.StrategyAssignmentSpec{"s": spec}
	st := newStrategyState("s")
	st.phase = pb.DeployPhase_DEPLOY_PHASE_HEALTHY
	st.runningArtifact = &pb.ArtifactRef{
		Type: pb.ArtifactType_ARTIFACT_TYPE_BINARY, Name: "strat",
		Version: "v2", Digest: "sha256:aaa", Uri: "http://old/v2",
	}
	r.actual["s"] = st

	r.reconcile()
	if cf.n != 0 || st.inflight != nil {
		t.Fatal("crash-restart same-digest retag must not fetch or deploy")
	}
	if st.runningArtifact.GetVersion() != "v3" {
		t.Fatalf("running version = %q, want v3", st.runningArtifact.GetVersion())
	}
	if st.proc == nil {
		t.Fatal("expected crash-restart to start a process")
	}
	if fd.starts() != 1 {
		t.Fatalf("starts = %d, want 1", fd.starts())
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

	// SkipBadVersion must be edge-triggered: repeated ticks over the same bad
	// version emit no further events (otherwise a single auto-rollback floods
	// the control plane forever).
	drainEvents(out)
	for i := 0; i < 10; i++ {
		r.reconcile()
	}
	if sawEvent(out, "SkipBadVersion") {
		t.Fatalf("SkipBadVersion must be emitted once, not on every tick")
	}
}

// A new version that crash-exits during HEALTH_CHECKING (before the readiness
// probe can promote it) must be restarted by reconcile() despite the deploy's
// inflight marker still being set, so the crash-loop budget is spent and auto-
// rollback fires. Regression: reconcileOne used to early-return on inflight,
// freezing restartCount at 1 and stalling in HEALTH_CHECKING forever. Unlike
// TestAutoRollbackOnCrashLoopAndSkipBadVersion this drives restarts through the
// real reconcile() path rather than restarting the process by hand.
func TestCrashLoopDuringHealthCheckingRestartsAndRollsBack(t *testing.T) {
	t0 := time.Unix(1000, 0)
	r, fd, mgr, clk, _ := newTestReconciler(t, t0)
	seedRelease(t, mgr, "s", "v1")
	seedRelease(t, mgr, "s", "v2")

	policy := &pb.DeployPolicy{Startsecs: 5, MaxCrashesInWindow: 2, EnableAutoRollback: true, HealthWindowSeconds: 120}
	spec := assignment("s", "v2", "sha256:v2", policy)
	r.desired = map[string]*pb.StrategyAssignmentSpec{"s": spec}

	// Deploy of v2 has just STARTED the new process: HEALTH_CHECKING with
	// inflight still set, a running process, and v1 available to roll back to.
	st := newStrategyState("s")
	st.phase = pb.DeployPhase_DEPLOY_PHASE_HEALTH_CHECKING
	st.inflight = &deployOp{target: artRef("v2", "sha256:v2"), cancel: func() {}}
	st.runningArtifact = artRef("v2", "sha256:v2")
	st.prevArtifact = artRef("v1", "sha256:v1")
	st.proc = mustStart(t, fd)
	st.proc.StartedAt = t0
	st.healthDeadline = t0.Add(120 * time.Second)
	r.actual["s"] = st

	// Each instance crashes ~immediately (lived < Startsecs). We never call
	// tick(), so readiness never promotes it: the only way out is crash-loop
	// rollback, which requires reconcile() to keep restarting it.
	for i := 0; i < 6 && st.phase != pb.DeployPhase_DEPLOY_PHASE_ROLLED_BACK; i++ {
		r.handleExit(processExit{strategy: "s", info: exitAt(st.proc, r.now().Add(time.Second))})
		clk.Advance(90 * time.Second) // past any backoff
		r.reconcile()
	}

	if st.phase != pb.DeployPhase_DEPLOY_PHASE_ROLLED_BACK {
		t.Fatalf("crash-looping new version must auto-roll-back; phase=%v restartCount=%d (inflight blocking restart?)",
			st.phase, st.restartCount)
	}
	if st.runningArtifact.GetVersion() != "v1" {
		t.Fatalf("running version = %q, want v1 after rollback", st.runningArtifact.GetVersion())
	}
}

func TestRollbackImpossibleStaysFailed(t *testing.T) {
	t0 := time.Unix(1000, 0)
	r, fd, _, fk, _ := newTestReconciler(t, t0)
	r.generation = 12
	policy := &pb.DeployPolicy{Startsecs: 5, MaxCrashesInWindow: 2, EnableAutoRollback: true, HealthWindowSeconds: 120}
	spec := assignment("s", "v1", "sha256:v1", policy)
	r.desired = map[string]*pb.StrategyAssignmentSpec{"s": spec}

	st := newStrategyState("s")
	st.phase = pb.DeployPhase_DEPLOY_PHASE_HEALTH_CHECKING
	st.inflight = &deployOp{target: artRef("v1", "sha256:v1"), cancel: func() {}}
	st.runningArtifact = artRef("v1", "sha256:v1")
	r.actual["s"] = st

	for i := 0; i < 3; i++ {
		proc := mustStart(t, fd)
		proc.StartedAt = t0
		st.proc = proc
		r.handleExit(processExit{strategy: "s", info: exitAt(proc, t0.Add(time.Second))})
	}

	if st.phase != pb.DeployPhase_DEPLOY_PHASE_FAILED {
		t.Fatalf("phase = %v, want FAILED", st.phase)
	}
	if st.failedAtGen != 12 {
		t.Fatalf("failedAtGen = %d, want 12", st.failedAtGen)
	}
	if st.lastError != "no previous version to roll back to" {
		t.Fatalf("lastError = %q", st.lastError)
	}

	starts := fd.starts()
	fk.Advance(2 * time.Minute)
	for i := 0; i < 5; i++ {
		r.reconcile()
	}
	if fd.starts() != starts {
		t.Fatalf("FAILED must not restart; starts %d → %d", starts, fd.starts())
	}
	if st.phase != pb.DeployPhase_DEPLOY_PHASE_FAILED {
		t.Fatalf("phase overwritten to %v", st.phase)
	}
}

func TestHealthyExitDemotesPhaseUntilRestart(t *testing.T) {
	t0 := time.Unix(1000, 0)
	r, fd, _, fk, _ := newTestReconciler(t, t0)
	r.generation = 7
	spec := assignment("s", "v1", "sha256:aaa", &pb.DeployPolicy{Startsecs: 5})
	r.desired = map[string]*pb.StrategyAssignmentSpec{"s": spec}

	st := newStrategyState("s")
	st.phase = pb.DeployPhase_DEPLOY_PHASE_HEALTHY
	st.runningArtifact = artRef("v1", "sha256:aaa")
	st.lastError = "stale from previous generation"
	proc := mustStart(t, fd)
	proc.StartedAt = t0
	st.proc = proc
	r.actual["s"] = st

	r.handleExit(processExit{strategy: "s", info: exitAt(proc, t0.Add(time.Second))})
	if st.phase != pb.DeployPhase_DEPLOY_PHASE_STARTING {
		t.Fatalf("phase = %v, want STARTING while dead", st.phase)
	}
	if st.lastError == "" || st.lastError == "stale from previous generation" {
		t.Fatalf("lastError should record the crash, got %q", st.lastError)
	}
	r.recomputeObservedGeneration()
	if r.observedGenA.Load() == 7 {
		t.Fatal("machine must not stay converged while the process is dead")
	}

	fk.Advance(2 * time.Second)
	r.reconcile()
	if st.phase != pb.DeployPhase_DEPLOY_PHASE_HEALTHY {
		t.Fatalf("phase = %v, want HEALTHY after restart", st.phase)
	}
	if st.proc == nil {
		t.Fatal("expected live process")
	}
	if st.lastError != "" {
		t.Fatalf("lastError should clear on successful start, got %q", st.lastError)
	}
}

func TestBeginDeployClearsLastError(t *testing.T) {
	r, _, _, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	spec := assignment("s", "v2", "sha256:v2", &pb.DeployPolicy{})
	spec.Artifact.Uri = "file://tmp/missing.sh"
	r.desired = map[string]*pb.StrategyAssignmentSpec{"s": spec}
	st := newStrategyState("s")
	st.lastError = "no previous version to roll back to"
	st.runningArtifact = artRef("v1", "sha256:v1")
	r.actual["s"] = st

	r.beginDeploy(spec, st)
	if st.lastError != "" {
		t.Fatalf("lastError = %q, want empty after beginDeploy", st.lastError)
	}
	waitWorkerPhase(t, r, pb.DeployPhase_DEPLOY_PHASE_FAILED)
}

func TestFailedDeployDoesNotRetryUntilGenerationChanges(t *testing.T) {
	r, _, _, _, out := newTestReconciler(t, time.Unix(1000, 0))
	spec := assignment("s", "v1", "sha256:aaa", &pb.DeployPolicy{})
	spec.Artifact.Uri = "file://tmp/missing.sh" // two-slash form; fetch would fail
	r.generation = 3
	r.desired = map[string]*pb.StrategyAssignmentSpec{"s": spec}
	st := newStrategyState("s")
	st.inflight = &deployOp{target: spec.Artifact, cancel: func() {}}
	r.actual["s"] = st

	r.applyWorkerEvent(workerEvent{
		strategy: "s",
		phase:    pb.DeployPhase_DEPLOY_PHASE_FAILED,
		err:      errString("fetch binary: open source tmp/missing.sh: no such file or directory"),
		artifact: spec.Artifact,
	})
	if st.phase != pb.DeployPhase_DEPLOY_PHASE_FAILED {
		t.Fatalf("phase = %v, want FAILED", st.phase)
	}
	if st.failedAtGen != 3 {
		t.Fatalf("failedAtGen = %d, want 3", st.failedAtGen)
	}
	if st.inflight != nil {
		t.Fatalf("inflight should be cleared on FAILED")
	}
	if !sawEvent(out, "DeployFailed") {
		t.Fatalf("expected DeployFailed event")
	}

	// Same generation: must stay FAILED, not redeploy.
	for i := 0; i < 5; i++ {
		r.reconcile()
	}
	if st.inflight != nil {
		t.Fatalf("FAILED must not restart deploy for the same generation")
	}
	if st.phase != pb.DeployPhase_DEPLOY_PHASE_FAILED {
		t.Fatalf("phase = %v, want still FAILED", st.phase)
	}

	// New generation (e.g. redeploy after fixing URI): may retry.
	r.generation = 4
	r.reconcile()
	if st.inflight == nil {
		t.Fatalf("new generation should begin a fresh deploy")
	}

	// beginDeploy ran its pipeline in a background goroutine (it fails on the
	// bad URI). Wait for it to finish so its release-dir writes complete before
	// t.TempDir cleanup, rather than racing it.
	waitWorkerPhase(t, r, pb.DeployPhase_DEPLOY_PHASE_FAILED)
}

func ociArtRef(version, digest string) *pb.ArtifactRef {
	return &pb.ArtifactRef{Type: pb.ArtifactType_ARTIFACT_TYPE_OCI_IMAGE, Name: "strat", Version: version, Digest: digest}
}

func crashWithOCIInitLog(t *testing.T, running *pb.ArtifactRef, logBody string) (lastError, warnLog string) {
	t.Helper()
	t0 := time.Unix(1000, 0)
	r, fd, mgr, _, _ := newTestReconciler(t, t0)
	var buf bytes.Buffer
	r.deps.Logger = slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn}))
	spec := assignment("s", running.GetVersion(), running.GetDigest(), &pb.DeployPolicy{Startsecs: 5})
	spec.Artifact = running
	r.desired = map[string]*pb.StrategyAssignmentSpec{"s": spec}
	st := newStrategyState("s")
	st.phase = pb.DeployPhase_DEPLOY_PHASE_STARTING
	st.runningArtifact = running
	proc := mustStart(t, fd)
	proc.StartedAt = t0
	st.proc = proc
	r.actual["s"] = st
	if _, err := mgr.EnsureWorkDir("s"); err != nil {
		t.Fatal(err)
	}
	logPath := driver.OCIInitLogPath(mgr.WorkDir("s"))
	if err := os.WriteFile(logPath, []byte(logBody), 0o644); err != nil {
		t.Fatal(err)
	}
	r.handleExit(processExit{strategy: "s", info: exitAt(proc, t0.Add(time.Second))})
	return st.lastError, buf.String()
}

func TestRecordStartCrashIgnoresEmptyOCIInitLog(t *testing.T) {
	lastError, warnLog := crashWithOCIInitLog(t, ociArtRef("v1", "sha256:aaa"), "")
	if lastError != "exited after start (code 0)" {
		t.Fatalf("empty oci-init.log must keep generic lastError, got %q", lastError)
	}
	if strings.Contains(warnLog, "oci-init failed") {
		t.Fatalf("empty log must not warn, got %q", warnLog)
	}
}

func TestRecordStartCrashUsesNonEmptyOCIInitLog(t *testing.T) {
	lastError, warnLog := crashWithOCIInitLog(t, ociArtRef("v1", "sha256:aaa"), "oci-init: exec /payload: exec format error\n")
	if lastError != "oci-init: exec /payload: exec format error" {
		t.Fatalf("lastError = %q", lastError)
	}
	if !strings.Contains(warnLog, "oci-init failed") {
		t.Fatalf("expected warn, got %q", warnLog)
	}
}

func TestRecordStartCrashIgnoresStaleLogForBinary(t *testing.T) {
	lastError, warnLog := crashWithOCIInitLog(t, artRef("v2", "sha256:bbb"), "stale oci-init from a previous OCI deploy\n")
	if lastError != "exited after start (code 0)" {
		t.Fatalf("binary crash must not use leftover oci-init.log, got %q", lastError)
	}
	if strings.Contains(warnLog, "oci-init failed") {
		t.Fatalf("binary crash must not warn about oci-init, got %q", warnLog)
	}
}

func TestRecordStartCrashReadsSiblingLogNotWorkDir(t *testing.T) {
	t0 := time.Unix(1000, 0)
	r, fd, mgr, _, _ := newTestReconciler(t, t0)
	spec := assignment("s", "v1", "sha256:aaa", &pb.DeployPolicy{Startsecs: 5})
	spec.Artifact = ociArtRef("v1", "sha256:aaa")
	r.desired = map[string]*pb.StrategyAssignmentSpec{"s": spec}
	st := newStrategyState("s")
	st.phase = pb.DeployPhase_DEPLOY_PHASE_STARTING
	st.runningArtifact = spec.Artifact
	proc := mustStart(t, fd)
	proc.StartedAt = t0
	st.proc = proc
	r.actual["s"] = st
	work, err := mgr.EnsureWorkDir("s")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(work, driver.OCIInitLogName), []byte("poisoned by payload\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(driver.OCIInitLogPath(work), []byte("oci-init: bind rootfs: no such file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.handleExit(processExit{strategy: "s", info: exitAt(proc, t0.Add(time.Second))})
	if st.lastError != "oci-init: bind rootfs: no such file" {
		t.Fatalf("must read sibling log, not bind-mounted work copy; got %q", st.lastError)
	}
}

// waitWorkerPhase drains workerCh until the given phase is observed, so tests
// that start a real deploy goroutine can wait for it to finish deterministically.
func waitWorkerPhase(t *testing.T, r *Reconciler, phase pb.DeployPhase) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		select {
		case ev := <-r.workerCh:
			if ev.phase == phase {
				return
			}
		case <-deadline:
			t.Fatalf("deploy worker did not reach %v", phase)
		}
	}
}

type errString string

func (e errString) Error() string { return string(e) }

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
