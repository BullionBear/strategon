package reconciler

import (
	"log/slog"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/agent/driver"
	"github.com/bullionbear/strategon/internal/agent/supervisefile"
	"google.golang.org/protobuf/proto"
)

// rebuildActualState loads the supervision snapshot and Adopt-s still-running
// strategy processes before the first reconcile (RECONCILER §10 step 4).
// Missing/corrupt files yield an empty actual map (cold start).
func (r *Reconciler) rebuildActualState() {
	if r.deps.BaseDir == "" {
		return
	}
	path := supervisefile.Path(r.deps.BaseDir)
	f, err := supervisefile.Load(path)
	if err != nil {
		r.logger().Warn("supervision file unreadable; starting empty", "path", path, "err", err)
		return
	}
	if f == nil {
		return
	}
	for name, entry := range f.Strategies {
		if entry.PID <= 0 {
			continue
		}
		proc, err := r.deps.Driver.Adopt(entry.PID, entry.StartTime, entry.StartedAt)
		if err != nil {
			r.logger().Info("adopt skipped", "strategy", name, "pid", entry.PID, "err", err)
			continue
		}
		st := newStrategyState(name)
		st.proc = proc
		st.startedAt = proc.StartedAt
		st.phase = parsePhase(entry.Phase)
		st.runningArtifact = artifactFromDTO(entry.RunningArtifact)
		st.runningConfig = artifactFromDTO(entry.RunningConfig)
		st.observedGen = entry.ObservedGeneration
		st.lastBadVersion = entry.LastBadVersion
		r.setCondition(st, conditionLive, pb.ConditionStatus_CONDITION_STATUS_TRUE, "Adopted", "")
		if st.phase == pb.DeployPhase_DEPLOY_PHASE_HEALTHY {
			r.setCondition(st, conditionReady, pb.ConditionStatus_CONDITION_STATUS_TRUE, "Adopted", "")
		}
		r.actual[name] = st
		// Launch exit watcher (same as installProcess, without overwriting phase).
		go func(strategy string, p *driver.Process) {
			info := r.deps.Driver.WatchExit(p, r.now)
			select {
			case r.exitCh <- processExit{strategy: strategy, info: info}:
			case <-r.ctx.Done():
			}
		}(name, proc)
		r.logger().Info("adopted strategy process", "strategy", name, "pid", proc.PID, "phase", st.phase.String())
	}
}

// persistSupervision writes the current actual map to disk (best-effort).
func (r *Reconciler) persistSupervision() {
	if r.deps.BaseDir == "" {
		return
	}
	f := &supervisefile.File{
		AgentVersion: r.deps.AgentVersion,
		Strategies:   map[string]supervisefile.Strategy{},
	}
	for name, st := range r.actual {
		if st.proc == nil {
			continue // only persist live process identities for takeover
		}
		f.Strategies[name] = supervisefile.Strategy{
			PID:                st.proc.PID,
			StartTime:          st.proc.StartTime,
			PGID:               st.proc.PGID,
			StartedAt:          st.proc.StartedAt,
			Phase:              st.phase.String(),
			RunningArtifact:    artifactToDTO(st.runningArtifact),
			RunningConfig:      artifactToDTO(st.runningConfig),
			ObservedGeneration: st.observedGen,
			LastBadVersion:     st.lastBadVersion,
		}
	}
	path := supervisefile.Path(r.deps.BaseDir)
	if err := supervisefile.Save(path, f); err != nil {
		r.logger().Warn("persist supervision failed", "path", path, "err", err)
	}
}

func (r *Reconciler) logger() *slog.Logger {
	if r.deps.Logger != nil {
		return r.deps.Logger
	}
	return slog.Default()
}

func parsePhase(s string) pb.DeployPhase {
	if v, ok := pb.DeployPhase_value[s]; ok {
		return pb.DeployPhase(v)
	}
	return pb.DeployPhase_DEPLOY_PHASE_HEALTHY
}

func artifactToDTO(a *pb.ArtifactRef) *supervisefile.Artifact {
	if a == nil {
		return nil
	}
	return &supervisefile.Artifact{
		Type:    a.GetType().String(),
		Name:    a.GetName(),
		Version: a.GetVersion(),
		Digest:  a.GetDigest(),
		URI:     a.GetUri(),
	}
}

func artifactFromDTO(a *supervisefile.Artifact) *pb.ArtifactRef {
	if a == nil {
		return nil
	}
	ref := &pb.ArtifactRef{
		Name:    a.Name,
		Version: a.Version,
		Digest:  a.Digest,
		Uri:     a.URI,
	}
	if v, ok := pb.ArtifactType_value[a.Type]; ok {
		ref.Type = pb.ArtifactType(v)
	}
	return proto.Clone(ref).(*pb.ArtifactRef)
}
