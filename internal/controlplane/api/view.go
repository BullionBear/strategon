package api

import (
	"sort"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func unixSec(sec int64) time.Time { return time.Unix(sec, 0).UTC() }

// BuildMachine assembles the human-facing Machine message, including the
// per-strategy StrategyView join of desired (spec) and actual (status).
// Convergence uses digest equality, matching the agent reconciler.
func BuildMachine(rec *store.MachineRecord) *pb.Machine {
	m := &pb.Machine{
		Metadata: &pb.ObjectMeta{
			Name:       rec.MachineID,
			Uid:        rec.MachineID,
			Generation: rec.Generation,
		},
		Reachable:    rec.Reachable,
		AgentVersion: rec.AgentVersion,
	}
	if rec.Register != nil {
		m.Spec = rec.Register.GetSpec()
		if m.Metadata.Name == "" {
			m.Metadata.Name = rec.Register.GetHostname()
		}
	}
	if rec.LastResources != nil {
		m.LastResources = rec.LastResources
	}
	if rec.LastHeartbeat > 0 {
		m.LastHeartbeat = timestamppb.New(unixSec(rec.LastHeartbeat))
	}
	m.Strategies = buildStrategyViews(rec)
	return m
}

func buildStrategyViews(rec *store.MachineRecord) []*pb.StrategyView {
	names := make([]string, 0, len(rec.Assignments))
	for n := range rec.Assignments {
		names = append(names, n)
	}
	// Also surface status-only strategies (retiring) if any.
	for n := range rec.Status {
		if _, ok := rec.Assignments[n]; !ok {
			names = append(names, n)
		}
	}
	sort.Strings(names)
	out := make([]*pb.StrategyView, 0, len(names))
	for _, name := range names {
		out = append(out, buildStrategyView(rec, name))
	}
	return out
}

func buildStrategyView(rec *store.MachineRecord, name string) *pb.StrategyView {
	v := &pb.StrategyView{Strategy: name, SpecGeneration: rec.Generation}
	if spec := rec.Assignments[name]; spec != nil {
		v.DesiredArtifact = spec.GetArtifact()
		v.DesiredConfig = spec.GetConfig()
	}
	if st := rec.Status[name]; st != nil {
		v.Phase = st.GetPhase()
		v.RunningArtifact = st.GetRunningArtifact()
		v.RunningConfig = st.GetRunningConfig()
		v.ObservedGeneration = st.GetObservedGeneration()
		v.Conditions = st.GetConditions()
		v.Pid = st.GetPid()
		v.RestartCount = st.GetRestartCount()
		v.LastError = st.GetLastError()
		v.LeaseHeld = st.GetLeaseHeld()
		v.LeaseExpiresAt = st.GetLeaseExpiresAt()
	}
	v.Converged = isConverged(v)
	return v
}

// isConverged mirrors reconciler versionMatches + HEALTHY (FRONTEND.md §1.2).
func isConverged(v *pb.StrategyView) bool {
	if v.GetPhase() != pb.DeployPhase_DEPLOY_PHASE_HEALTHY {
		return false
	}
	if v.GetDesiredArtifact() == nil || v.GetRunningArtifact() == nil {
		return false
	}
	if v.GetDesiredArtifact().GetDigest() == "" {
		return false
	}
	if v.GetDesiredArtifact().GetDigest() != v.GetRunningArtifact().GetDigest() {
		return false
	}
	return v.GetDesiredConfig().GetDigest() == v.GetRunningConfig().GetDigest()
}
