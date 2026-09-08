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

func TestIsConvergedRequiresMatchingVersion(t *testing.T) {
	v := &pb.StrategyView{
		Phase:           pb.DeployPhase_DEPLOY_PHASE_HEALTHY,
		Pid:             42,
		DesiredArtifact: &pb.ArtifactRef{Version: "v3", Digest: "sha256:aaa", Uri: "s3://bucket/v3"},
		RunningArtifact: &pb.ArtifactRef{Version: "v2", Digest: "sha256:aaa", Uri: "http://old/v2"},
	}
	if isConverged(v) {
		t.Fatal("same digest with stale version must not be converged")
	}
	v.RunningArtifact.Version = "v3"
	if !isConverged(v) {
		t.Fatal("matching digest + version should be converged")
	}

	v.DesiredConfig = &pb.ArtifactRef{Version: "c2", Digest: "sha256:cfg"}
	v.RunningConfig = &pb.ArtifactRef{Version: "c1", Digest: "sha256:cfg"}
	if isConverged(v) {
		t.Fatal("same config digest with stale config version must not be converged")
	}
	v.RunningConfig.Version = "c2"
	if !isConverged(v) {
		t.Fatal("matching config digest + version should be converged")
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
