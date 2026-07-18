package reconciler

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"syscall"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/agent/driver"
	"github.com/bullionbear/strategon/internal/agent/supervisor"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// beginDeploy starts the deploy state machine for a new target version. The IO
// steps run in a worker goroutine; the main loop only records phase (§4).
func (r *Reconciler) beginDeploy(spec *pb.StrategyAssignmentSpec, st *strategyState) {
	ctx, cancel := context.WithCancel(r.ctx)
	st.inflight = &deployOp{target: spec.GetArtifact(), config: spec.GetConfig(), cancel: cancel}
	st.phase = pb.DeployPhase_DEPLOY_PHASE_PENDING
	st.warnedBadVersion = "" // a real deploy is starting; allow a fresh skip warning later
	// Remember the currently-running version so rollback is O(1) (no download).
	st.prevArtifact = st.runningArtifact
	st.prevConfig = st.runningConfig
	oldProc := st.proc
	r.emitEvent(st.strategy, pb.EventSeverity_EVENT_SEVERITY_INFO, "DeployStarted",
		fmt.Sprintf("deploying %s", spec.GetArtifact().GetVersion()))
	go r.runDeploy(ctx, spec, oldProc)
}

// runDeploy executes the download→verify→drain→switch→start pipeline, emitting
// a workerEvent at each transition. It never mutates reconciler state directly.
func (r *Reconciler) runDeploy(ctx context.Context, spec *pb.StrategyAssignmentSpec, oldProc *driver.Process) {
	strat := spec.GetStrategy()
	art := spec.GetArtifact()
	cfg := spec.GetConfig()
	send := func(p pb.DeployPhase, err error, proc *driver.Process) {
		select {
		case r.workerCh <- workerEvent{strategy: strat, phase: p, err: err, proc: proc, artifact: art, config: cfg}:
		case <-ctx.Done():
		}
	}

	send(pb.DeployPhase_DEPLOY_PHASE_DOWNLOADING, nil, nil)
	if ctx.Err() != nil {
		return
	}
	if err := r.deps.Artifacts.Download(ctx, strat, art, cfg); err != nil {
		send(pb.DeployPhase_DEPLOY_PHASE_FAILED, err, nil)
		return
	}

	send(pb.DeployPhase_DEPLOY_PHASE_VERIFYING, nil, nil)
	if err := r.deps.Artifacts.Verify(strat, art); err != nil {
		send(pb.DeployPhase_DEPLOY_PHASE_FAILED, err, nil)
		return
	}

	// Only now do we tear down the old process (download/verify failure keeps
	// the old process running, RECONCILER §11).
	send(pb.DeployPhase_DEPLOY_PHASE_DRAINING, nil, nil)
	if oldProc != nil {
		r.gracefulStop(oldProc, spec.GetDeployPolicy().GetStopGraceSeconds())
	}
	if ctx.Err() != nil {
		return
	}

	send(pb.DeployPhase_DEPLOY_PHASE_SWITCHING, nil, nil)
	if err := r.deps.Artifacts.SwitchTo(strat, art.GetVersion()); err != nil {
		send(pb.DeployPhase_DEPLOY_PHASE_FAILED, err, nil)
		return
	}

	// STARTING: fork/exec the new version.
	sp := r.buildStartSpec(spec)
	proc, err := r.deps.Driver.Start(sp, r.now())
	if err != nil {
		send(pb.DeployPhase_DEPLOY_PHASE_ROLLING_BACK, err, nil)
		return
	}
	send(pb.DeployPhase_DEPLOY_PHASE_HEALTH_CHECKING, nil, proc)
}

// applyWorkerEvent advances phase in the main loop based on worker progress.
func (r *Reconciler) applyWorkerEvent(ev workerEvent) {
	st := r.actual[ev.strategy]
	if st == nil || st.inflight == nil {
		return // deploy was cancelled/withdrawn
	}
	if ev.artifact.GetDigest() != st.inflight.target.GetDigest() {
		return // event from a superseded deploy
	}

	switch ev.phase {
	case pb.DeployPhase_DEPLOY_PHASE_DOWNLOADING,
		pb.DeployPhase_DEPLOY_PHASE_VERIFYING,
		pb.DeployPhase_DEPLOY_PHASE_SWITCHING:
		st.phase = ev.phase

	case pb.DeployPhase_DEPLOY_PHASE_DRAINING:
		st.phase = ev.phase
		st.stopping = true // the old process's exit is now expected

	case pb.DeployPhase_DEPLOY_PHASE_HEALTH_CHECKING:
		st.stopping = false
		st.proc = nil // old process already drained
		st.runningArtifact = ev.artifact
		st.runningConfig = ev.config
		spec := r.desired[ev.strategy]
		if spec == nil {
			return
		}
		r.installProcess(spec, st, ev.proc)
		st.phase = pb.DeployPhase_DEPLOY_PHASE_HEALTH_CHECKING
		st.healthDeadline = r.now().Add(healthWindow(spec))
		st.backoff.Reset()

	case pb.DeployPhase_DEPLOY_PHASE_ROLLING_BACK:
		spec := r.desired[ev.strategy]
		if ev.err != nil {
			st.lastError = ev.err.Error()
		}
		if spec != nil {
			r.beginRollback(spec, st)
		}

	case pb.DeployPhase_DEPLOY_PHASE_FAILED:
		st.phase = pb.DeployPhase_DEPLOY_PHASE_FAILED
		st.stopping = false
		if ev.err != nil {
			st.lastError = ev.err.Error()
		}
		st.failedAtGen = r.generation
		st.inflight = nil
		r.emitEvent(ev.strategy, pb.EventSeverity_EVENT_SEVERITY_ERROR, "DeployFailed", st.lastError)
	}
}

// beginRollback re-points the symlink to the previous good version and restarts
// (O(1), no download), marking the failed version bad so reconcile() stops
// pulling it back up (§4.1, §6.3).
func (r *Reconciler) beginRollback(spec *pb.StrategyAssignmentSpec, st *strategyState) {
	badVersion := spec.GetArtifact().GetVersion()
	if st.inflight != nil {
		badVersion = st.inflight.target.GetVersion()
		st.inflight.cancel()
		st.inflight = nil
	}
	st.lastBadVersion = badVersion

	if st.proc != nil {
		// Stop the failed process before switching back.
		r.gracefulStop(st.proc, spec.GetDeployPolicy().GetStopGraceSeconds())
		st.proc = nil
	}

	if st.prevArtifact == nil {
		st.phase = pb.DeployPhase_DEPLOY_PHASE_FAILED
		st.lastError = "no previous version to roll back to"
		r.emitEvent(st.strategy, pb.EventSeverity_EVENT_SEVERITY_ERROR, "RollbackImpossible", st.lastError)
		return
	}

	if err := r.deps.Artifacts.SwitchTo(st.strategy, st.prevArtifact.GetVersion()); err != nil {
		st.phase = pb.DeployPhase_DEPLOY_PHASE_FAILED
		st.lastError = err.Error()
		r.emitEvent(st.strategy, pb.EventSeverity_EVENT_SEVERITY_ERROR, "RollbackFailed", err.Error())
		return
	}
	st.runningArtifact = st.prevArtifact
	st.runningConfig = st.prevConfig

	proc, err := r.deps.Driver.Start(r.buildStartSpec(spec), r.now())
	if err != nil {
		st.phase = pb.DeployPhase_DEPLOY_PHASE_FAILED
		st.lastError = err.Error()
		r.emitEvent(st.strategy, pb.EventSeverity_EVENT_SEVERITY_ERROR, "RollbackFailed", err.Error())
		return
	}
	r.installProcess(spec, st, proc)
	st.phase = pb.DeployPhase_DEPLOY_PHASE_ROLLED_BACK
	st.backoff.Reset()
	r.emitEvent(st.strategy, pb.EventSeverity_EVENT_SEVERITY_ERROR, "AutoRollback",
		fmt.Sprintf("rolled back %s -> %s", badVersion, st.prevArtifact.GetVersion()))
}

// spawnDrain gracefully stops a strategy's process in a worker, then (for
// retirement) removes it from state. The exit is marked expected via stopping.
func (r *Reconciler) spawnDrain(st *strategyState, spec *pb.StrategyAssignmentSpec, retire bool) {
	st.stopping = true
	st.phase = pb.DeployPhase_DEPLOY_PHASE_DRAINING
	proc := st.proc
	grace := int32(0)
	if spec != nil {
		grace = spec.GetDeployPolicy().GetStopGraceSeconds()
	} else if s := r.desired[st.strategy]; s != nil {
		grace = s.GetDeployPolicy().GetStopGraceSeconds()
	}
	go func() {
		r.gracefulStop(proc, grace)
		// The exit watcher will deliver the exit; handleExit finalizes removal.
		_ = retire
	}()
}

// gracefulStop runs the shared SIGTERM->grace->SIGKILL sequence (RECONCILER §7)
// against a process handle using the driver.
func (r *Reconciler) gracefulStop(proc *driver.Process, graceSecs int32) {
	if proc == nil {
		return
	}
	grace := time.Duration(graceSecs) * time.Second
	if grace <= 0 {
		grace = 10 * time.Second
	}
	seq := supervisor.StopSequence{
		// Drain hook (cancel-orders/flatten over unix socket) is a follow-up;
		// left nil for the foundation.
		Drain:  nil,
		Signal: func(sig syscall.Signal) error { return r.deps.Driver.Signal(proc, sig) },
		Exited: func() bool { return !r.processAlive(proc) },
		Grace:  grace,
		Sleep:  time.Sleep,
		Now:    r.deps.Clock.Now,
	}
	seq.Run()
}

// processAlive reports whether the driver-managed process is still running.
// The exec driver defends against PID reuse via starttime; here we approximate
// via a fresh WatchExit-independent check by re-adopting.
func (r *Reconciler) processAlive(proc *driver.Process) bool {
	p, err := r.deps.Driver.Adopt(proc.PID, proc.StartTime, proc.StartedAt)
	if err != nil {
		return false
	}
	_ = p
	return true
}

func (r *Reconciler) setCondition(st *strategyState, typ string, status pb.ConditionStatus, reason, msg string) {
	c := st.conditions[typ]
	now := timestamppb.New(r.now())
	if c == nil || c.GetStatus() != status {
		st.conditions[typ] = &pb.Condition{
			Type:           typ,
			Status:         status,
			Reason:         reason,
			Message:        msg,
			LastTransition: now,
		}
		return
	}
	c.Reason = reason
	c.Message = msg
}

func (r *Reconciler) emitEvent(strategy string, sev pb.EventSeverity, reason, msg string) {
	r.send(&pb.AgentMessage{
		Payload: &pb.AgentMessage_Event{Event: &pb.Event{
			Timestamp: timestamppb.New(r.now()),
			Severity:  sev,
			Strategy:  strategy,
			Reason:    reason,
			Message:   msg,
		}},
	})
}

// reportStatusIfChanged sends a StatusReport only when the rendered status
// differs from the last one (debounce).
func (r *Reconciler) reportStatusIfChanged() {
	report := r.buildStatusReport()
	key := statusKey(report)
	if key == r.lastReport {
		return
	}
	r.lastReport = key
	r.send(&pb.AgentMessage{Payload: &pb.AgentMessage_StatusReport{StatusReport: report}})
}

func (r *Reconciler) buildStatusReport() *pb.StatusReport {
	names := make([]string, 0, len(r.actual))
	for name := range r.actual {
		names = append(names, name)
	}
	sort.Strings(names)
	assignments := make([]*pb.StrategyAssignmentStatus, 0, len(names))
	for _, name := range names {
		st := r.actual[name]
		conds := make([]*pb.Condition, 0, len(st.conditions))
		ctypes := make([]string, 0, len(st.conditions))
		for t := range st.conditions {
			ctypes = append(ctypes, t)
		}
		sort.Strings(ctypes)
		for _, t := range ctypes {
			conds = append(conds, st.conditions[t])
		}
		var pid int32
		var startedAt *timestamppb.Timestamp
		if st.proc != nil {
			pid = int32(st.proc.PID)
			startedAt = timestamppb.New(st.proc.StartedAt)
		}
		assignments = append(assignments, &pb.StrategyAssignmentStatus{
			Strategy:           name,
			Phase:              st.phase,
			ObservedGeneration: st.observedGen,
			RunningArtifact:    st.runningArtifact,
			RunningConfig:      st.runningConfig,
			Conditions:         conds,
			Pid:                pid,
			RestartCount:       st.restartCount,
			StartedAt:          startedAt,
			LastError:          st.lastError,
		})
	}
	return &pb.StatusReport{ObservedGeneration: r.observedGenA.Load(), Assignments: assignments}
}

func (r *Reconciler) send(msg *pb.AgentMessage) {
	if r.deps.Out == nil {
		return
	}
	select {
	case r.deps.Out <- msg:
	default: // never block the loop
	}
}

func statusKey(sr *pb.StatusReport) string {
	var b strings.Builder
	fmt.Fprintf(&b, "g=%d;", sr.GetObservedGeneration())
	for _, a := range sr.GetAssignments() {
		fmt.Fprintf(&b, "%s:phase=%d,ver=%s,pid=%d,rc=%d,err=%s;",
			a.GetStrategy(), a.GetPhase(), a.GetRunningArtifact().GetVersion(),
			a.GetPid(), a.GetRestartCount(), a.GetLastError())
		for _, c := range a.GetConditions() {
			fmt.Fprintf(&b, "%s=%d,", c.GetType(), c.GetStatus())
		}
	}
	return b.String()
}
