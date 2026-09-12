// Package view joins desired assignment specs with agent-reported status.
// One definition of converged is shared by the human API, CLI, and
// cluster controllers.
package view

import (
	"sort"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
)

func unixSec(sec int64) time.Time { return time.Unix(sec, 0).UTC() }

// BuildMachine assembles the human-facing Machine message, including the
// per-strategy StrategyView join of desired (spec) and actual (status).
// Convergence uses digest + version equality, matching the agent reconciler.
// st may be nil; when set, fencing-lease fields come from the CP lease store
// (authoritative) rather than agent-reported status.
func BuildMachine(rec *store.MachineRecord, st store.Store) *pb.Machine {
	m := &pb.Machine{
		Metadata: &pb.ObjectMeta{
			Name:       rec.MachineID,
			Uid:        rec.MachineID,
			Generation: rec.Generation,
		},
		Reachable:         rec.Reachable,
		AgentVersion:      rec.AgentVersion,
		AgentBuildVersion: rec.AgentBuildVersion,
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
	if len(rec.LastProcesses) > 0 {
		m.LastProcesses = make([]*pb.ProcessMetrics, len(rec.LastProcesses))
		for i, p := range rec.LastProcesses {
			m.LastProcesses[i] = proto.Clone(p).(*pb.ProcessMetrics)
		}
	}
	if rec.LastHeartbeat > 0 {
		m.LastHeartbeat = timestamppb.New(unixSec(rec.LastHeartbeat))
	}
	m.Strategies = BuildStrategyViews(rec, st)
	return m
}

// BuildStrategyViews returns one StrategyView per assigned or retiring strategy.
func BuildStrategyViews(rec *store.MachineRecord, st store.Store) []*pb.StrategyView {
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
	procByStrat := map[string]*pb.ProcessMetrics{}
	for _, p := range rec.LastProcesses {
		if p.GetStrategy() != "" {
			procByStrat[p.GetStrategy()] = p
		}
	}
	deployedAt := latestDeployTimes(st, rec.MachineID)
	out := make([]*pb.StrategyView, 0, len(names))
	for _, name := range names {
		out = append(out, BuildStrategyView(rec, name, st, procByStrat[name], deployedAt[name]))
	}
	return out
}

// BuildStrategyView joins one strategy's desired spec with observed status.
func BuildStrategyView(rec *store.MachineRecord, name string, st store.Store, proc *pb.ProcessMetrics, deployedAt *timestamppb.Timestamp) *pb.StrategyView {
	v := &pb.StrategyView{Strategy: name, SpecGeneration: rec.Generation}
	if spec := rec.Assignments[name]; spec != nil {
		v.DesiredArtifact = spec.GetArtifact()
		v.DesiredConfig = spec.GetConfig()
		v.Schedules = spec.GetSchedules()
		v.Stopped = spec.GetStopped()
		v.VolumeMounts = spec.GetVolumeMounts()
		v.CaptureStdio = spec.GetCaptureStdio()
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
		v.StartedAt = status.GetStartedAt()
	}
	if proc != nil {
		v.CpuPercent = proc.GetCpuPercent()
		v.RssBytes = proc.GetRssBytes()
		v.NumFds = proc.GetNumFds()
		if v.Pid == 0 {
			v.Pid = proc.GetPid()
		}
	}
	if deployedAt != nil {
		v.DeployedAt = deployedAt
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
	v.Converged = IsConverged(v)
	return v
}

func latestDeployTimes(st store.Store, machineID string) map[string]*timestamppb.Timestamp {
	out := map[string]*timestamppb.Timestamp{}
	if st == nil {
		return out
	}
	for _, e := range st.ListAudit(machineID, "") {
		switch e.GetAction() {
		case "Deploy", "SetDeployment", "Rollback", "ApplyAssignment":
		default:
			continue
		}
		strat := e.GetStrategy()
		if strat == "" || out[strat] != nil {
			continue // ListAudit is newest-first
		}
		out[strat] = e.GetTimestamp()
	}
	return out
}

// IsConverged mirrors reconciler convergence: HEALTHY + live process + digest
// and version match, or an intentionally halted deployment settled at STOPPED.
// Same digest with a stale version (http→s3 retag before the agent promotes
// the label) is not converged — that used to lie to Rollback and the UI.
func IsConverged(v *pb.StrategyView) bool {
	if v.GetStopped() {
		return v.GetPhase() == pb.DeployPhase_DEPLOY_PHASE_STOPPED
	}
	if v.GetPhase() != pb.DeployPhase_DEPLOY_PHASE_HEALTHY {
		return false
	}
	if !AssignmentLive(v) {
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
	if v.GetDesiredArtifact().GetVersion() != v.GetRunningArtifact().GetVersion() {
		return false
	}
	if v.GetDesiredConfig().GetDigest() != v.GetRunningConfig().GetDigest() {
		return false
	}
	return v.GetDesiredConfig().GetVersion() == v.GetRunningConfig().GetVersion()
}

// AssignmentLive reports whether the process is alive, preferring the Live
// condition over a possibly-stale pid.
func AssignmentLive(v *pb.StrategyView) bool {
	for _, c := range v.GetConditions() {
		if c.GetType() == "Live" {
			return c.GetStatus() == pb.ConditionStatus_CONDITION_STATUS_TRUE
		}
	}
	// Older agents omit the Live condition; pid is a fallback only then.
	// A stale ProcessMetrics pid must not override Live=FALSE.
	return v.GetPid() > 0
}

// ReadyConditionTrue reports whether StrategyView carries Ready == TRUE.
func ReadyConditionTrue(v *pb.StrategyView) bool {
	for _, c := range v.GetConditions() {
		if c.GetType() == "Ready" {
			return c.GetStatus() == pb.ConditionStatus_CONDITION_STATUS_TRUE
		}
	}
	return false
}
