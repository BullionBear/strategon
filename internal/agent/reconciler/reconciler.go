// Package reconciler implements the agent's level-triggered convergence loop
// (RECONCILER.md). A single goroutine owns all mutable state and serializes
// four event sources — new DesiredState, process exits, deploy-worker events,
// and a unified tick — each of which runs the same path: update local state,
// call reconcile(), diff desired vs actual, and act. There are no per-command
// handlers; events are just "something changed, recompute" triggers.
package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"sync/atomic"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/agent/artifact"
	"github.com/bullionbear/strategon/internal/agent/driver"
	"github.com/bullionbear/strategon/internal/agent/health"
	"github.com/bullionbear/strategon/internal/agent/supervisor"
	"github.com/bullionbear/strategon/internal/clock"
)

const (
	conditionLive            = "Live"
	conditionReady           = "Ready"
	conditionBusinessHealthy = "BusinessHealthy"
)

// Deps are the injected collaborators for a Reconciler.
type Deps struct {
	Driver    driver.Driver
	Artifacts *artifact.Manager
	Health    health.Checker
	Clock     clock.Clock

	// Out receives northbound messages (StatusReport, Event). Buffered by the
	// caller; the reconciler never blocks on it (drops on full to preserve the
	// non-blocking loop invariant).
	Out chan<- *pb.AgentMessage

	// ReadyEndpoint resolves a strategy's readiness endpoint (unix socket
	// path). May be nil (no endpoint => ready once live).
	ReadyEndpoint func(strategy string) string

	// TickInterval is the unified time-wheel interval (default 1s).
	TickInterval time.Duration

	// Jitter is injected into backoff (nil disables jitter for tests).
	Jitter func(time.Duration) time.Duration

	// CronRand returns an integer in [0, n) for cron schedule jitter.
	// Nil uses math/rand (non-crypto; only for multi-machine stagger).
	CronRand func(n int32) int32

	// BaseDir is the agent --base directory; when set, supervision state is
	// persisted under <BaseDir>/agent/supervision.json for restart takeover.
	BaseDir string

	// AgentVersion is stamped into the supervision file header.
	AgentVersion int

	// Logger for adopt/persist diagnostics (optional).
	Logger *slog.Logger
}

// Reconciler is the agent core.
type Reconciler struct {
	desired    map[string]*pb.StrategyAssignmentSpec
	actual     map[string]*strategyState
	generation int64

	desiredCh chan *pb.DesiredState
	exitCh    chan processExit
	workerCh  chan workerEvent
	healthCh  chan healthResult

	deps         Deps
	tickInterval time.Duration
	ctx          context.Context

	lastReport   string
	observedGenA atomic.Int64
}

// New constructs a Reconciler.
func New(deps Deps) *Reconciler {
	if deps.Health == nil {
		deps.Health = health.AlwaysReady{}
	}
	if deps.Clock == nil {
		deps.Clock = clock.Real{}
	}
	tick := deps.TickInterval
	if tick <= 0 {
		tick = time.Second
	}
	return &Reconciler{
		desired:      map[string]*pb.StrategyAssignmentSpec{},
		actual:       map[string]*strategyState{},
		desiredCh:    make(chan *pb.DesiredState, 8),
		exitCh:       make(chan processExit, 16),
		workerCh:     make(chan workerEvent, 32),
		healthCh:     make(chan healthResult, 32),
		deps:         deps,
		tickInterval: tick,
	}
}

// SubmitDesired hands a new DesiredState snapshot to the loop (called by the
// stream client goroutine).
func (r *Reconciler) SubmitDesired(ds *pb.DesiredState) {
	select {
	case r.desiredCh <- ds:
	case <-r.ctx.Done():
	}
}

// ObservedGeneration returns the generation the agent has converged to, for the
// stream client to stamp on heartbeats. Safe for concurrent reads.
func (r *Reconciler) ObservedGeneration() int64 { return r.observedGenA.Load() }

// Run drives the loop until ctx is cancelled.
func (r *Reconciler) Run(ctx context.Context) {
	r.ctx = ctx
	r.rebuildActualState()
	tick := r.deps.Clock.Ticker(r.tickInterval)
	defer tick.Stop()
	for {
		select {
		case ds := <-r.desiredCh:
			r.applyDesired(ds)
		case ex := <-r.exitCh:
			r.handleExit(ex)
		case ev := <-r.workerCh:
			r.applyWorkerEvent(ev)
		case hr := <-r.healthCh:
			r.applyHealthResult(hr)
		case now := <-tick.C():
			r.tick(now)
		case <-ctx.Done():
			r.shutdown()
			return
		}
		r.reconcile()
		r.reportStatusIfChanged()
		r.persistSupervision()
	}
}

func (r *Reconciler) now() time.Time { return r.deps.Clock.Now() }

// applyDesired overwrites the local desired copy and cancels any in-flight
// deploy whose target no longer matches (deploy withdrawal, RECONCILER §12).
func (r *Reconciler) applyDesired(ds *pb.DesiredState) {
	if ds == nil {
		return
	}
	r.generation = ds.GetGeneration()
	next := map[string]*pb.StrategyAssignmentSpec{}
	for _, a := range ds.GetAssignments() {
		next[a.GetStrategy()] = a
	}
	// Cancel in-flight deploys that are now targeting a stale version.
	for name, st := range r.actual {
		if st.inflight == nil {
			continue
		}
		spec, want := next[name]
		if !want || spec.GetArtifact().GetDigest() != st.inflight.target.GetDigest() {
			st.inflight.cancel()
			st.inflight = nil
		}
	}
	r.desired = next
}

// reconcile is the sole convergence entry point (RECONCILER §3).
func (r *Reconciler) reconcile() {
	for name, spec := range r.desired {
		st := r.actual[name]
		if st == nil {
			st = newStrategyState(name)
			r.actual[name] = st
		}
		r.reconcileOne(spec, st)
	}
	for name, st := range r.actual {
		if _, want := r.desired[name]; !want {
			r.retireStrategy(st)
		}
	}
	r.recomputeObservedGeneration()
}

func (r *Reconciler) reconcileOne(spec *pb.StrategyAssignmentSpec, st *strategyState) {
	st.stopGraceSeconds = spec.GetDeployPolicy().GetStopGraceSeconds()
	if st.backoff.Blocked(r.now()) {
		return // backoff not elapsed; tick will wake us
	}
	if st.inflight != nil {
		// A deploy is in flight. During the download→verify→switch pipeline the
		// main loop must not touch process state. But once the deploy has
		// STARTED the new process (HEALTH_CHECKING) the worker goroutine is
		// done and supervision is the main loop's job: if that new version
		// crash-exits before it is promoted to HEALTHY, restart it here so the
		// crash-loop budget is spent and auto-rollback can fire. Without this a
		// fast-crashing new version stalls forever — inflight (cleared only by
		// markHealthy/rollback) would otherwise block every restart, freezing
		// the crash counter below the rollback threshold. Backoff (checked
		// above) paces the restarts.
		if st.phase == pb.DeployPhase_DEPLOY_PHASE_HEALTH_CHECKING &&
			st.proc == nil && versionMatches(spec, st) {
			r.startProcess(spec, st)
		}
		return // otherwise wait for worker events
	}
	// FAILED is terminal for this desired generation (RECONCILER §4.1): report
	// the error and wait for the next desired change (new generation) rather
	// than hammering download forever on a bad URI/digest.
	if st.phase == pb.DeployPhase_DEPLOY_PHASE_FAILED && st.failedAtGen == r.generation {
		return
	}
	switch {
	case versionMatches(spec, st) && st.phase == pb.DeployPhase_DEPLOY_PHASE_HEALTHY && st.proc != nil:
		st.observedGen = r.generation
		return // steady state

	case versionMatches(spec, st) && st.proc == nil:
		// Same version, process gone (crashed): restart, not redeploy.
		r.startProcess(spec, st)

	case !versionMatches(spec, st):
		if spec.GetArtifact().GetVersion() == st.lastBadVersion {
			// Edge-triggered: warn once when we start skipping this bad version,
			// not on every tick. Otherwise a single auto-rollback floods the
			// control plane with ~1 event/sec forever (the ROLLED_BACK state
			// permanently satisfies this branch until desired changes).
			if st.warnedBadVersion != st.lastBadVersion {
				st.warnedBadVersion = st.lastBadVersion
				r.emitEvent(st.strategy, pb.EventSeverity_EVENT_SEVERITY_WARNING, "SkipBadVersion",
					fmt.Sprintf("skipping known-bad version %s", st.lastBadVersion))
			}
			return
		}
		r.beginDeploy(spec, st)
	}
}

// retireStrategy drains and removes a strategy no longer in desired (§5).
func (r *Reconciler) retireStrategy(st *strategyState) {
	if st.inflight != nil {
		st.inflight.cancel()
		st.inflight = nil
	}
	if st.proc == nil {
		delete(r.actual, st.strategy)
		return
	}
	if st.stopping {
		return // drain already in progress
	}
	r.spawnDrain(st, nil, true)
}

// startProcess forks/execs the strategy on the CURRENT symlinked binary and
// begins supervising it. Called in the main loop (RECONCILER §6.1) for
// crash-restart and rollback; deploy STARTING happens in the worker.
func (r *Reconciler) startProcess(spec *pb.StrategyAssignmentSpec, st *strategyState) {
	sp := r.buildStartSpec(spec)
	proc, err := r.deps.Driver.Start(sp, r.now())
	if err != nil {
		st.lastError = err.Error()
		st.phase = pb.DeployPhase_DEPLOY_PHASE_FAILED
		r.emitEvent(st.strategy, pb.EventSeverity_EVENT_SEVERITY_ERROR, "StartFailed", err.Error())
		return
	}
	r.installProcess(spec, st, proc)
	st.phase = pb.DeployPhase_DEPLOY_PHASE_HEALTH_CHECKING
	st.healthDeadline = r.now().Add(healthWindow(spec))
}

// installProcess wires a freshly-started process into state and launches its
// exit watcher (single-writer: only the main loop launches watchers).
func (r *Reconciler) installProcess(spec *pb.StrategyAssignmentSpec, st *strategyState, proc *driver.Process) {
	st.proc = proc
	st.startedAt = proc.StartedAt
	st.stopping = false
	r.setCondition(st, conditionLive, pb.ConditionStatus_CONDITION_STATUS_TRUE, "Started", "")
	go func(strategy string, p *driver.Process) {
		info := r.deps.Driver.WatchExit(p, r.now)
		select {
		case r.exitCh <- processExit{strategy: strategy, info: info}:
		case <-r.ctx.Done():
		}
	}(st.strategy, proc)
}

func (r *Reconciler) buildStartSpec(spec *pb.StrategyAssignmentSpec) driver.StartSpec {
	env := make([]string, 0, len(spec.GetEnv()))
	for k, v := range spec.GetEnv() {
		env = append(env, k+"="+v)
	}
	limits := spec.GetLimits()
	return driver.StartSpec{
		Strategy:      spec.GetStrategy(),
		BinaryPath:    r.deps.Artifacts.CurrentBinaryPath(spec.GetStrategy()),
		Args:          spec.GetArgs(),
		Env:           env,
		WorkDir:       r.deps.Artifacts.StrategyDir(spec.GetStrategy()),
		CPUMillicores: limits.GetCpuMillicores(),
		MemoryBytes:   limits.GetMemoryBytes(),
		MaxOpenFiles:  limits.GetMaxOpenFiles(),
	}
}

// handleExit processes a process-exit notification (RECONCILER §6.3).
func (r *Reconciler) handleExit(ex processExit) {
	st := r.actual[ex.strategy]
	if st == nil || st.proc == nil || st.proc.PID != ex.info.PID || st.proc.StartTime != ex.info.StartTime {
		return // stale notification (e.g. PID reuse) — ignore
	}
	lived := ex.info.At.Sub(st.proc.StartedAt)
	st.proc = nil
	r.setCondition(st, conditionLive, pb.ConditionStatus_CONDITION_STATUS_FALSE, "Exited", "")

	if st.stopping {
		st.stopping = false
		if _, want := r.desired[st.strategy]; !want {
			delete(r.actual, st.strategy)
		}
		return // expected stop (drain/retire/deploy-drain)
	}

	spec := r.desired[st.strategy]
	if spec == nil {
		return // being retired; reconcile's retire path will clean up
	}
	policy := spec.GetDeployPolicy()
	if supervisor.CrashedOnStart(lived, int(policy.GetStartsecs())) {
		st.backoff.RecordCrash(r.now(), r.deps.Jitter)
		st.restartCount++
		if st.phase == pb.DeployPhase_DEPLOY_PHASE_HEALTH_CHECKING &&
			st.backoff.Consecutive > int(policy.GetMaxCrashesInWindow()) &&
			policy.GetEnableAutoRollback() {
			r.emitEvent(st.strategy, pb.EventSeverity_EVENT_SEVERITY_ERROR, "CrashLoop",
				fmt.Sprintf("%d crashes in health window", st.backoff.Consecutive))
			r.beginRollback(spec, st)
			return
		}
		// else: exponential backoff; reconcile() restarts when the tick elapses.
	} else {
		st.backoff.Reset() // lived long enough: healthy run that exited, restart clean
	}
}

// tick drives time-based work: health-window evaluation, async readiness
// probing, and local cron schedule evaluation (ARCHITECTURE §10). Backoff
// wakeups are handled by reconcile() running after every tick.
func (r *Reconciler) tick(now time.Time) {
	for _, st := range r.actual {
		if st.phase != pb.DeployPhase_DEPLOY_PHASE_HEALTH_CHECKING || st.proc == nil {
			continue
		}
		if now.After(st.healthDeadline) {
			spec := r.desired[st.strategy]
			if spec != nil && spec.GetDeployPolicy().GetEnableAutoRollback() && st.inflight != nil {
				r.emitEvent(st.strategy, pb.EventSeverity_EVENT_SEVERITY_ERROR, "HealthTimeout",
					"readiness not achieved within health window")
				r.beginRollback(spec, st)
			} else {
				// No rollback configured: accept as healthy once the window passes.
				r.markHealthy(st)
			}
			continue
		}
		r.probeReadiness(st)
	}
	r.tickCron(now)
}

// probeReadiness launches an async readiness probe (non-blocking loop).
func (r *Reconciler) probeReadiness(st *strategyState) {
	if st.probeInflight {
		return
	}
	endpoint := ""
	if r.deps.ReadyEndpoint != nil {
		endpoint = r.deps.ReadyEndpoint(st.strategy)
	}
	st.probeInflight = true
	go func(strategy, endpoint string) {
		res := r.deps.Health.Ready(r.ctx, strategy, endpoint)
		select {
		case r.healthCh <- healthResult{strategy: strategy, status: res.Status, reason: res.Reason, message: res.Message}:
		case <-r.ctx.Done():
		}
	}(st.strategy, endpoint)
}

func (r *Reconciler) applyHealthResult(hr healthResult) {
	st := r.actual[hr.strategy]
	if st == nil {
		return
	}
	st.probeInflight = false
	r.setCondition(st, conditionReady, hr.status, hr.reason, hr.message)
	if st.phase == pb.DeployPhase_DEPLOY_PHASE_HEALTH_CHECKING &&
		hr.status == pb.ConditionStatus_CONDITION_STATUS_TRUE {
		r.markHealthy(st)
	}
}

// markHealthy promotes a strategy to HEALTHY steady state.
func (r *Reconciler) markHealthy(st *strategyState) {
	st.phase = pb.DeployPhase_DEPLOY_PHASE_HEALTHY
	st.backoff.Reset()
	if st.inflight != nil {
		st.inflight = nil
		r.emitEvent(st.strategy, pb.EventSeverity_EVENT_SEVERITY_INFO, "DeployHealthy",
			fmt.Sprintf("version %s healthy", st.runningArtifact.GetVersion()))
	}
	st.observedGen = r.generation
}

func (r *Reconciler) recomputeObservedGeneration() {
	converged := true
	for name, spec := range r.desired {
		st := r.actual[name]
		if st == nil || !versionMatches(spec, st) || st.phase != pb.DeployPhase_DEPLOY_PHASE_HEALTHY {
			converged = false
			break
		}
	}
	if converged {
		r.observedGenA.Store(r.generation)
	}
}

func (r *Reconciler) shutdown() {
	// Agent SIGTERM: do NOT kill strategy processes (setsid-detached). Persist
	// supervision so the next agent can Adopt; strategies keep running
	// (RECONCILER §7 / §10).
	r.persistSupervision()
}

// versionMatches compares desired vs actual by content digest (artifact +
// config). Content addressing is the only trustworthy equality (RECONCILER §3).
func versionMatches(spec *pb.StrategyAssignmentSpec, st *strategyState) bool {
	if st.runningArtifact == nil {
		return false
	}
	if st.runningArtifact.GetDigest() != spec.GetArtifact().GetDigest() {
		return false
	}
	return spec.GetConfig().GetDigest() == st.runningConfig.GetDigest()
}

func healthWindow(spec *pb.StrategyAssignmentSpec) time.Duration {
	w := spec.GetDeployPolicy().GetHealthWindowSeconds()
	if w <= 0 {
		w = 30
	}
	return time.Duration(w) * time.Second
}
