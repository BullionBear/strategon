package reconciler

import (
	"context"
	"testing"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/agent/health"
)

type staticHealth struct{ res health.Result }

func (s staticHealth) Ready(context.Context, string, string) health.Result { return s.res }

func readyCond(st *strategyState) pb.ConditionStatus {
	c := st.conditions[conditionReady]
	if c == nil {
		return pb.ConditionStatus_CONDITION_STATUS_UNSPECIFIED
	}
	return c.GetStatus()
}

func healthCheckingState(r *Reconciler, spec *pb.StrategyAssignmentSpec, deadline time.Time) *strategyState {
	r.desired = map[string]*pb.StrategyAssignmentSpec{spec.GetStrategy(): spec}
	st := newStrategyState(spec.GetStrategy())
	st.phase = pb.DeployPhase_DEPLOY_PHASE_HEALTH_CHECKING
	st.runningArtifact = spec.GetArtifact()
	st.healthDeadline = deadline
	st.inflight = &deployOp{target: spec.GetArtifact(), cancel: func() {}}
	r.actual[spec.GetStrategy()] = st
	return st
}

func drainHealth(r *Reconciler) {
	for {
		select {
		case hr := <-r.healthCh:
			r.applyHealthResult(hr)
		default:
			return
		}
	}
}

func TestProbeKeepsHealthCheckingWhileFailing(t *testing.T) {
	t0 := time.Unix(1000, 0)
	r, fd, _, _, _ := newTestReconciler(t, t0)
	r.deps.Health = staticHealth{res: health.Result{Status: pb.ConditionStatus_CONDITION_STATUS_FALSE, Reason: "Unreachable"}}
	spec := assignment("s", "v1", "sha256:aaa", &pb.DeployPolicy{HealthWindowSeconds: 30})
	spec.Readiness = &pb.ReadinessProbe{Endpoint: "http://127.0.0.1:1/healthz"}
	st := healthCheckingState(r, spec, t0.Add(30*time.Second))
	st.proc = mustStart(t, fd)
	r.tick(t0)
	time.Sleep(20 * time.Millisecond)
	drainHealth(r)
	if st.phase != pb.DeployPhase_DEPLOY_PHASE_HEALTH_CHECKING {
		t.Fatalf("phase=%v", st.phase)
	}
	if readyCond(st) != pb.ConditionStatus_CONDITION_STATUS_FALSE {
		t.Fatalf("Ready=%v", readyCond(st))
	}
}

func TestProbeTimeoutWithoutRollbackFails(t *testing.T) {
	t0 := time.Unix(1000, 0)
	r, fd, _, _, _ := newTestReconciler(t, t0)
	spec := assignment("s", "v1", "sha256:aaa", &pb.DeployPolicy{HealthWindowSeconds: 5, EnableAutoRollback: false})
	spec.Readiness = &pb.ReadinessProbe{Endpoint: "http://127.0.0.1:1/healthz"}
	st := healthCheckingState(r, spec, t0.Add(5*time.Second))
	st.proc = mustStart(t, fd)
	r.tick(t0.Add(6 * time.Second))
	if st.phase != pb.DeployPhase_DEPLOY_PHASE_FAILED {
		t.Fatalf("phase=%v, want FAILED (not HEALTHY)", st.phase)
	}
}

func TestProbeTimeoutRollbackRestoresPrevious(t *testing.T) {
	t0 := time.Unix(1000, 0)
	r, fd, mgr, _, _ := newTestReconciler(t, t0)
	seedRelease(t, mgr, "s", "v1")
	seedRelease(t, mgr, "s", "v2")
	spec := assignment("s", "v2", "sha256:v2", &pb.DeployPolicy{HealthWindowSeconds: 5, EnableAutoRollback: true})
	spec.Readiness = &pb.ReadinessProbe{Endpoint: "http://127.0.0.1:1/healthz"}
	st := healthCheckingState(r, spec, t0.Add(5*time.Second))
	st.prevArtifact = artRef("v1", "sha256:v1")
	st.proc = mustStart(t, fd)
	r.tick(t0.Add(6 * time.Second))
	if st.phase != pb.DeployPhase_DEPLOY_PHASE_ROLLED_BACK {
		t.Fatalf("phase=%v, want ROLLED_BACK", st.phase)
	}
}

func TestProbeTimeoutRollbackImpossible(t *testing.T) {
	t0 := time.Unix(1000, 0)
	r, fd, _, _, _ := newTestReconciler(t, t0)
	spec := assignment("s", "v1", "sha256:aaa", &pb.DeployPolicy{HealthWindowSeconds: 5, EnableAutoRollback: true})
	spec.Readiness = &pb.ReadinessProbe{Endpoint: "http://127.0.0.1:1/healthz"}
	st := healthCheckingState(r, spec, t0.Add(5*time.Second))
	st.proc = mustStart(t, fd)
	r.tick(t0.Add(6 * time.Second))
	if st.phase != pb.DeployPhase_DEPLOY_PHASE_FAILED {
		t.Fatalf("phase=%v, want FAILED", st.phase)
	}
}

func TestProbeSuccessMarksHealthy(t *testing.T) {
	t0 := time.Unix(1000, 0)
	r, fd, _, _, _ := newTestReconciler(t, t0)
	r.deps.Health = staticHealth{res: health.Result{Status: pb.ConditionStatus_CONDITION_STATUS_TRUE, Reason: "Healthy"}}
	spec := assignment("s", "v1", "sha256:aaa", &pb.DeployPolicy{HealthWindowSeconds: 30})
	spec.Readiness = &pb.ReadinessProbe{Endpoint: "http://127.0.0.1:8222/healthz"}
	st := healthCheckingState(r, spec, t0.Add(30*time.Second))
	st.proc = mustStart(t, fd)
	r.tick(t0)
	time.Sleep(20 * time.Millisecond)
	drainHealth(r)
	if st.phase != pb.DeployPhase_DEPLOY_PHASE_HEALTHY {
		t.Fatalf("phase=%v", st.phase)
	}
	if readyCond(st) != pb.ConditionStatus_CONDITION_STATUS_TRUE {
		t.Fatalf("Ready=%v", readyCond(st))
	}
}

func TestHealthyProbeFailureDemotesReady(t *testing.T) {
	t0 := time.Unix(1000, 0)
	r, fd, _, _, _ := newTestReconciler(t, t0)
	r.deps.Health = staticHealth{res: health.Result{Status: pb.ConditionStatus_CONDITION_STATUS_FALSE, Reason: "Unreachable"}}
	spec := assignment("s", "v1", "sha256:aaa", &pb.DeployPolicy{HealthWindowSeconds: 30})
	spec.Readiness = &pb.ReadinessProbe{Endpoint: "http://127.0.0.1:8222/healthz"}
	r.desired = map[string]*pb.StrategyAssignmentSpec{spec.GetStrategy(): spec}
	st := newStrategyState(spec.GetStrategy())
	st.phase = pb.DeployPhase_DEPLOY_PHASE_HEALTHY
	st.runningArtifact = spec.GetArtifact()
	st.proc = mustStart(t, fd)
	r.setCondition(st, conditionReady, pb.ConditionStatus_CONDITION_STATUS_TRUE, "Healthy", "")
	r.actual[spec.GetStrategy()] = st

	r.tick(t0)
	time.Sleep(20 * time.Millisecond)
	drainHealth(r)
	if st.phase != pb.DeployPhase_DEPLOY_PHASE_HEALTHY {
		t.Fatalf("phase=%v, want HEALTHY", st.phase)
	}
	if readyCond(st) != pb.ConditionStatus_CONDITION_STATUS_FALSE {
		t.Fatalf("Ready=%v, want FALSE", readyCond(st))
	}
}

func TestNoProbeTimeoutStillHealthy(t *testing.T) {
	t0 := time.Unix(1000, 0)
	r, fd, _, _, _ := newTestReconciler(t, t0)
	spec := assignment("s", "v1", "sha256:aaa", &pb.DeployPolicy{HealthWindowSeconds: 5, EnableAutoRollback: false})
	st := healthCheckingState(r, spec, t0.Add(5*time.Second))
	st.proc = mustStart(t, fd)
	r.tick(t0.Add(6 * time.Second))
	if st.phase != pb.DeployPhase_DEPLOY_PHASE_HEALTHY {
		t.Fatalf("phase=%v, want HEALTHY (no probe)", st.phase)
	}
}
