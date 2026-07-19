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
// st may be nil; when set, fencing-lease fields come from the CP lease store
// (authoritative) rather than agent-reported status.
func BuildMachine(rec *store.MachineRecord, st store.Store) *pb.Machine {
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
	m.Strategies = buildStrategyViews(rec, st)
	return m
}

func buildStrategyViews(rec *store.MachineRecord, st store.Store) []*pb.StrategyView {
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
		out = append(out, buildStrategyView(rec, name, st))
	}
	return out
}

func buildStrategyView(rec *store.MachineRecord, name string, st store.Store) *pb.StrategyView {
	v := &pb.StrategyView{Strategy: name, SpecGeneration: rec.Generation}
	if spec := rec.Assignments[name]; spec != nil {
		v.DesiredArtifact = spec.GetArtifact()
		v.DesiredConfig = spec.GetConfig()
		v.Schedules = spec.GetSchedules()
	}
	if status := rec.Status[name]; status != nil {
		v.Phase = status.GetPhase()
		v.RunningArtifact = status.GetRunningArtifact()
		v.RunningConfig = status.GetRunningConfig()
		v.ObservedGeneration = status.GetObservedGeneration()
		v.Conditions = status.GetConditions()
		v.Pid = status.GetPid()
		v.RestartCount = status.GetRestartCount()
		v.LastError = status.GetLastError()
		v.LeaseHeld = status.GetLeaseHeld()
		v.LeaseExpiresAt = status.GetLeaseExpiresAt()
	}
	// Control-plane lease store is authoritative for fencing state.
	if st != nil {
		if info, ok := st.GetLease(name); ok {
			heldUntil := info.ExpiresAt.Add(st.LeaseMarginCP())
			if info.MachineID == rec.MachineID && !time.Now().After(heldUntil) {
				v.LeaseHeld = true
				v.LeaseExpiresAt = timestamppb.New(info.ExpiresAt)
			} else {
				v.LeaseHeld = false
				v.LeaseExpiresAt = nil
			}
		}
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
