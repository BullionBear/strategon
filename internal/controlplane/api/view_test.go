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
		t.Fatal("HEALTHY + pid should be converged")
	}
	v.Pid = 0
	v.Conditions = []*pb.Condition{{
		Type:   "Live",
		Status: pb.ConditionStatus_CONDITION_STATUS_FALSE,
		Reason: "Exited",
	}}
	if isConverged(v) {
		t.Fatal("HEALTHY + Live=FALSE must not be converged")
	}
	v.Conditions[0].Status = pb.ConditionStatus_CONDITION_STATUS_TRUE
	if !isConverged(v) {
		t.Fatal("HEALTHY + Live=TRUE should be converged")
	}
}
