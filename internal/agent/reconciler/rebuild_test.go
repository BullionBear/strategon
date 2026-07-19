package reconciler

import (
	"context"
	"testing"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/agent/artifact"
	"github.com/bullionbear/strategon/internal/agent/driver"
	"github.com/bullionbear/strategon/internal/agent/health"
	"github.com/bullionbear/strategon/internal/agent/supervisefile"
	"github.com/bullionbear/strategon/internal/clock"
)

func TestRebuildActualStateAdoptsWithoutRestart(t *testing.T) {
	base := t.TempDir()
	fd := newFakeDriver()
	t.Cleanup(fd.closeAll)
	mgr := artifact.NewManager(base, artifact.LocalFetcher{})
	out := make(chan *pb.AgentMessage, 64)
	fk := clock.NewFake(time.Unix(1000, 0))

	r1 := New(Deps{
		Driver: fd, Artifacts: mgr, Health: health.AlwaysReady{}, Clock: fk,
		Out: out, BaseDir: base, AgentVersion: 1,
	})
	r1.ctx = context.Background()

	proc, err := fd.Start(driver.StartSpec{Strategy: "s", BinaryPath: "/bin/true"}, fk.Now())
	if err != nil {
		t.Fatal(err)
	}
	st := newStrategyState("s")
	st.phase = pb.DeployPhase_DEPLOY_PHASE_HEALTHY
	st.runningArtifact = artRef("v1", "sha256:aaa")
	st.observedGen = 3
	st.proc = proc
	r1.actual["s"] = st
	r1.persistSupervision()

	startsBefore := fd.starts()

	r2 := New(Deps{
		Driver: fd, Artifacts: mgr, Health: health.AlwaysReady{}, Clock: fk,
		Out: out, BaseDir: base, AgentVersion: 2,
	})
	r2.ctx = context.Background()
	r2.rebuildActualState()

	got := r2.actual["s"]
	if got == nil || got.proc == nil {
		t.Fatal("expected adopted process")
	}
	if got.proc.PID != proc.PID || got.phase != pb.DeployPhase_DEPLOY_PHASE_HEALTHY {
		t.Fatalf("adopted state: pid=%d phase=%v", got.proc.PID, got.phase)
	}
	if got.runningArtifact.GetDigest() != "sha256:aaa" || got.observedGen != 3 {
		t.Fatalf("artifact/obs: %+v gen=%d", got.runningArtifact, got.observedGen)
	}

	r2.generation = 3
	r2.desired = map[string]*pb.StrategyAssignmentSpec{
		"s": assignment("s", "v1", "sha256:aaa", &pb.DeployPolicy{Startsecs: 1}),
	}
	r2.reconcile()
	if fd.starts() != startsBefore {
		t.Fatalf("takeover must not Start again; starts before=%d after=%d", startsBefore, fd.starts())
	}
}

func TestRebuildSkipsDeadOrMismatchedPID(t *testing.T) {
	base := t.TempDir()
	fd := newFakeDriver()
	t.Cleanup(fd.closeAll)
	path := supervisefile.Path(base)
	if err := supervisefile.Save(path, &supervisefile.File{
		AgentVersion: 1,
		Strategies: map[string]supervisefile.Strategy{
			"s": {
				PID: 999, StartTime: 1, Phase: "DEPLOY_PHASE_HEALTHY",
				RunningArtifact: &supervisefile.Artifact{Version: "v1", Digest: "sha256:aaa"},
			},
		},
	}); err != nil {
		t.Fatal(err)
	}

	r := New(Deps{
		Driver: fd, Artifacts: artifact.NewManager(base, artifact.LocalFetcher{}),
		Health: health.AlwaysReady{}, Clock: clock.NewFake(time.Unix(1, 0)),
		Out: make(chan *pb.AgentMessage, 8), BaseDir: base,
	})
	r.ctx = context.Background()
	r.rebuildActualState()
	if len(r.actual) != 0 {
		t.Fatalf("dead pid should not be adopted: %+v", r.actual)
	}
}
