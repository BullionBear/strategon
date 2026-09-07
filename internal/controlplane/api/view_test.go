package api

import (
	"testing"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

func TestIsConvergedRequiresLiveProcess(t *testing.T) {
	v := &pb.StrategyView{
		Phase:           pb.DeployPhase_DEPLOY_PHASE_HEALTHY,
		DesiredArtifact: &pb.ArtifactRef{Digest: "sha256:aaa"},
		RunningArtifact: &pb.ArtifactRef{Digest: "sha256:aaa"},
	}
	if isConverged(v) {
		t.Fatal("HEALTHY without pid or Live must not be converged")
	}
	v.Pid = 42
	if !isConverged(v) {
		t.Fatal("HEALTHY + pid (no Live condition) should be converged")
	}
	v.Conditions = []*pb.Condition{{
		Type:   "Live",
		Status: pb.ConditionStatus_CONDITION_STATUS_FALSE,
		Reason: "Exited",
	}}
	if isConverged(v) {
		t.Fatal("HEALTHY + Live=FALSE must not be converged even when pid is set")
	}
	v.Pid = 0
	if isConverged(v) {
		t.Fatal("HEALTHY + Live=FALSE must not be converged")
	}
	v.Conditions[0].Status = pb.ConditionStatus_CONDITION_STATUS_TRUE
	if !isConverged(v) {
		t.Fatal("HEALTHY + Live=TRUE should be converged")
	}
}

func TestAssignmentLivePrefersLiveCondition(t *testing.T) {
	v := &pb.StrategyView{
		Pid: 99,
		Conditions: []*pb.Condition{{
			Type:   "Live",
			Status: pb.ConditionStatus_CONDITION_STATUS_FALSE,
		}},
	}
	if assignmentLive(v) {
		t.Fatal("stale pid must not override Live=FALSE")
	}
	v.Conditions[0].Status = pb.ConditionStatus_CONDITION_STATUS_TRUE
	if !assignmentLive(v) {
		t.Fatal("Live=TRUE should be live")
	}
	v.Conditions = nil
	if !assignmentLive(v) {
		t.Fatal("pid is a fallback when no Live condition is present")
	}
	v.Pid = 0
	if assignmentLive(v) {
		t.Fatal("no pid and no Live must not be live")
	}
}
