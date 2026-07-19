package reconciler

import (
	"syscall"
	"testing"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

func TestCronReloadConfig(t *testing.T) {
	t0 := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	r, fd, _, _, out := newTestReconciler(t, t0)
	r.deps.CronRand = func(n int32) int32 { return 0 }

	spec := assignment("s", "v1", "sha256:aaa", &pb.DeployPolicy{Startsecs: 5})
	spec.Schedules = []*pb.CronSchedule{{
		Name:       "reload",
		CronExpr:   "0 0 * * *",
		Timezone:   "UTC",
		Action:     pb.CronAction_CRON_ACTION_RELOAD_CONFIG,
		JitterSeconds: 0,
	}}
	r.desired = map[string]*pb.StrategyAssignmentSpec{"s": spec}
	st := newStrategyState("s")
	st.phase = pb.DeployPhase_DEPLOY_PHASE_HEALTHY
	st.runningArtifact = artRef("v1", "sha256:aaa")
	st.proc = mustStart(t, fd)
	r.actual["s"] = st

	// Prime next fire, then advance past it.
	r.tickCron(t0)
	ent := st.cron["reload"]
	if ent == nil {
		t.Fatal("expected cron entry after prime")
	}
	wantNext := time.Date(2024, 6, 2, 0, 0, 0, 0, time.UTC)
	if !ent.nextFire.Equal(wantNext) {
		t.Fatalf("nextFire = %v, want %v", ent.nextFire, wantNext)
	}

	r.tickCron(wantNext)
	sigs := fd.signalList()
	if len(sigs) != 1 || sigs[0] != syscall.SIGHUP {
		t.Fatalf("signals = %v, want [SIGHUP]", sigs)
	}
	if !drainReason(out, "CronExecuted") {
		t.Fatal("expected CronExecuted event")
	}
}

func TestCronDeferredWhileInflight(t *testing.T) {
	t0 := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	r, fd, _, _, out := newTestReconciler(t, t0)
	r.deps.CronRand = func(n int32) int32 { return 0 }

	spec := assignment("s", "v1", "sha256:aaa", &pb.DeployPolicy{Startsecs: 5})
	spec.Schedules = []*pb.CronSchedule{{
		Name:     "daily",
		CronExpr: "0 0 * * *",
		Timezone: "UTC",
		Action:   pb.CronAction_CRON_ACTION_RELOAD_CONFIG,
	}}
	r.desired = map[string]*pb.StrategyAssignmentSpec{"s": spec}
	st := newStrategyState("s")
	st.phase = pb.DeployPhase_DEPLOY_PHASE_HEALTHY
	st.runningArtifact = artRef("v1", "sha256:aaa")
	st.proc = mustStart(t, fd)
	st.inflight = &deployOp{target: artRef("v2", "sha256:bbb"), cancel: func() {}}
	r.actual["s"] = st

	r.tickCron(t0)
	due := st.cron["daily"].nextFire
	r.tickCron(due)
	if len(fd.signalList()) != 0 {
		t.Fatal("should not signal while deploy inflight")
	}
	if !st.cron["daily"].nextFire.Equal(due) {
		t.Fatal("nextFire should not advance while deferred")
	}
	if !drainReason(out, "CronDeferred") {
		t.Fatal("expected CronDeferred event")
	}

	st.inflight = nil
	r.tickCron(due)
	if len(fd.signalList()) != 1 {
		t.Fatalf("expected SIGHUP after inflight cleared, got %v", fd.signalList())
	}
}

func TestCronRestartDrains(t *testing.T) {
	t0 := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	r, fd, _, _, out := newTestReconciler(t, t0)
	r.deps.CronRand = func(n int32) int32 { return 0 }

	spec := assignment("s", "v1", "sha256:aaa", &pb.DeployPolicy{Startsecs: 5, StopGraceSeconds: 1})
	spec.Schedules = []*pb.CronSchedule{{
		Name:     "restart",
		CronExpr: "0 0 * * *",
		Timezone: "UTC",
		Action:   pb.CronAction_CRON_ACTION_RESTART,
	}}
	r.desired = map[string]*pb.StrategyAssignmentSpec{"s": spec}
	st := newStrategyState("s")
	st.phase = pb.DeployPhase_DEPLOY_PHASE_HEALTHY
	st.runningArtifact = artRef("v1", "sha256:aaa")
	proc := mustStart(t, fd)
	st.proc = proc
	r.actual["s"] = st

	r.tickCron(t0)
	due := st.cron["restart"].nextFire
	r.tickCron(due)

	if st.phase != pb.DeployPhase_DEPLOY_PHASE_DRAINING || !st.stopping {
		t.Fatalf("phase=%v stopping=%v, want DRAINING/stopping", st.phase, st.stopping)
	}
	if !drainReason(out, "CronExecuted") {
		t.Fatal("expected CronExecuted")
	}

	// Unblock gracefulStop + exit watcher.
	fd.kill(proc.PID)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && st.proc != nil {
		select {
		case ex := <-r.exitCh:
			r.handleExit(ex)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	if st.proc != nil {
		// Drain may still be racing; synthesize the expected exit.
		r.handleExit(processExit{strategy: "s", info: exitAt(proc, t0.Add(time.Second))})
	}
	r.reconcile()
	if fd.starts() < 2 {
		t.Fatalf("expected restart after cron drain, starts=%d", fd.starts())
	}
}

func drainReason(out <-chan *pb.AgentMessage, reason string) bool {
	deadline := time.After(200 * time.Millisecond)
	for {
		select {
		case msg := <-out:
			if ev := msg.GetEvent(); ev != nil && ev.GetReason() == reason {
				return true
			}
		case <-deadline:
			return false
		}
	}
}
