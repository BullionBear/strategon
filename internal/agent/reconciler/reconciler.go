// Package reconciler implements the agent's level-triggered convergence loop.
// A single goroutine owns all mutable state and serializes
// event sources — new DesiredState, process exits, deploy-worker events,
// shared-file fetch workers, health results, and a unified tick — each of
// which runs the same path: update local state, call reconcile(), diff desired
// vs actual, and act. There are no per-command handlers; events are just
// "something changed, recompute" triggers. Shared-file HTTP fetches never run
// on the main loop (same non-blocking invariant as deploy downloads).
package reconciler

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/agent/artifact"
	"github.com/bullionbear/strategon/internal/agent/driver"
	"github.com/bullionbear/strategon/internal/agent/health"
	"github.com/bullionbear/strategon/internal/agent/supervisor"
	"github.com/bullionbear/strategon/internal/clock"
	"google.golang.org/protobuf/proto"
)

var placeholderRE = regexp.MustCompile(`\$\{([A-Za-z_][A-Za-z0-9_]*)\}`)

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

	// SharedRetention is how many store entries to keep per shared-file name
	// (including the live symlink target). Default 3 when <= 0.
	SharedRetention int

	// ReleaseRetention is how many release dirs to keep per strategy
	// (including current). Default 3 when <= 0.
	ReleaseRetention int

	// Logger for adopt/persist diagnostics (optional).
	Logger *slog.Logger
}

// Reconciler is the agent core.
type Reconciler struct {
	desired    map[string]*pb.StrategyAssignmentSpec
	actual     map[string]*strategyState
	generation int64

	desiredShared    map[string]*pb.SharedFileSpec
	sharedGeneration int64
	sharedActual     map[string]*sharedFileState

	desiredCh chan *pb.DesiredState
	exitCh    chan processExit
	workerCh  chan workerEvent
	sharedCh  chan sharedWorkerEvent
	healthCh  chan healthResult

	deps         Deps
	tickInterval time.Duration
	ctx          context.Context

	lastReport   string
	observedGenA atomic.Int64
	// processTargets is a read-only snapshot for the telemetry collector.
	// Updated only by the reconciler goroutine; safe for concurrent Load.
	processTargets atomic.Value // []ProcessTarget
}

// ProcessTarget is a read-only view of a managed process for resource sampling.
// Published by the reconciler; consumed by the telemetry collector (never mutates state).
type ProcessTarget struct {
	Strategy     string
	PID          int32
	Alive        bool
	RestartCount int32
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
	if deps.Artifacts != nil && deps.ReleaseRetention > 0 {
		deps.Artifacts.ReleaseRetention = deps.ReleaseRetention
	}
	return &Reconciler{
		desired:       map[string]*pb.StrategyAssignmentSpec{},
		actual:        map[string]*strategyState{},
		desiredShared: map[string]*pb.SharedFileSpec{},
		sharedActual:  map[string]*sharedFileState{},
		desiredCh:     make(chan *pb.DesiredState, 8),
		exitCh:        make(chan processExit, 16),
		workerCh:      make(chan workerEvent, 32),
		sharedCh:      make(chan sharedWorkerEvent, 32),
		healthCh:      make(chan healthResult, 32),
		deps:          deps,
		tickInterval:  tick,
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

// ProcessTargets returns a snapshot of managed processes for telemetry sampling.
// Safe for concurrent reads; never returns nil.
func (r *Reconciler) ProcessTargets() []ProcessTarget {
	v, _ := r.processTargets.Load().([]ProcessTarget)
	if v == nil {
		return nil
	}
	out := make([]ProcessTarget, len(v))
	copy(out, v)
	return out
}

func (r *Reconciler) publishProcessTargets() {
	out := make([]ProcessTarget, 0, len(r.actual))
	for name, st := range r.actual {
		t := ProcessTarget{Strategy: name, RestartCount: st.restartCount}
		if st.proc != nil {
			// Presence of a handle is enough here; the collector discovers dead
			// PIDs via /proc and exit events clear st.proc on the reconciler.
			t.PID = int32(st.proc.PID)
			t.Alive = true
		}
		out = append(out, t)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Strategy < out[j].Strategy })
	r.processTargets.Store(out)
}

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
		case ev := <-r.sharedCh:
			r.applySharedWorkerEvent(ev)
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
		r.publishProcessTargets()
		r.persistSupervision()
	}
}

func (r *Reconciler) now() time.Time { return r.deps.Clock.Now() }

// applyDesired overwrites the local desired copy and cancels any in-flight
// deploy whose target no longer matches (deploy withdrawal).
func (r *Reconciler) applyDesired(ds *pb.DesiredState) {
	if ds == nil {
		return
	}
	r.generation = ds.GetGeneration()
	r.applyDesiredShared(ds)
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

// reconcile is the sole convergence entry point.
func (r *Reconciler) reconcile() {
	// Initiate shared convergence first; assignment start/deploy is gated on
	// sharedPresent (absent only) so a fresh machine does not start before the
	// catalog lands — stale digests do not freeze the machine.
	r.reconcileShared()
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
	if spec.GetStopped() {
		r.reconcileStopped(spec, st)
		return
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
			if !r.awaitSharedReady(st) {
				return
			}
			r.startProcess(spec, st, true)
		}
		return // otherwise wait for worker events
	}
	// FAILED is terminal for this desired generation: report
	// the error and wait for the next desired change (new generation) rather
	// than hammering download forever on a bad URI/digest.
	if st.phase == pb.DeployPhase_DEPLOY_PHASE_FAILED && st.failedAtGen == r.generation {
		return
	}
	switch {
	case versionMatches(spec, st) && st.phase == pb.DeployPhase_DEPLOY_PHASE_HEALTHY && st.proc != nil:
		// Same bytes, possibly a new version/uri (http→s3 retag). Do not
		// re-fetch; just advance the running label so status matches desired.
		r.promoteRunningLabels(spec, st)
		st.observedGen = r.generation
		return // steady state

	case versionMatches(spec, st) && st.proc == nil:
		r.promoteRunningLabels(spec, st)
		if !r.awaitSharedReady(st) {
			return
		}
		if st.phase == pb.DeployPhase_DEPLOY_PHASE_STOPPED {
			// Resume in place: re-run the health window (STARTING → HEALTH_CHECKING).
			r.startProcess(spec, st, true)
		} else {
			// Crash restart: keep HEALTHY (crash-loop budget already spent during
			// the initial health window; no need to re-enter it).
			r.startProcess(spec, st, false)
		}

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
		if !r.awaitSharedReady(st) {
			return
		}
		r.beginDeploy(spec, st)
	}
}

// reconcileStopped drains the process when desired.stopped is set, but never
// deletes the strategy from actual state (unlike retireStrategy). WorkDir,
// release, and last-running artifact are retained for browse / fast resume.
func (r *Reconciler) reconcileStopped(spec *pb.StrategyAssignmentSpec, st *strategyState) {
	if st.inflight != nil {
		st.inflight.cancel()
		st.inflight = nil
	}
	if st.proc != nil {
		if !st.stopping {
			r.spawnDrain(st, spec, false)
		}
		return
	}
	st.phase = pb.DeployPhase_DEPLOY_PHASE_STOPPED
	st.stopping = false
	st.lastError = ""
	st.observedGen = r.generation
}

// retireStrategy drains and removes a strategy no longer in desired.
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
// begins supervising it. Called in the main loop for crash-restart, resume, and
// rollback; deploy STARTING happens in the worker.
//
// When healthCheck is true the process enters STARTING → HEALTH_CHECKING (resume
// / post-deploy crash during the window). When false (steady-state crash
// restart) the process is live again so phase returns to HEALTHY.
func (r *Reconciler) startProcess(spec *pb.StrategyAssignmentSpec, st *strategyState, healthCheck bool) {
	if healthCheck {
		st.phase = pb.DeployPhase_DEPLOY_PHASE_STARTING
	}
	launch := st.runningArtifact
	if launch == nil {
		launch = spec.GetArtifact()
	}
	sp, err := r.buildStartSpec(spec, launch)
	if err != nil {
		st.lastError = err.Error()
		st.phase = pb.DeployPhase_DEPLOY_PHASE_FAILED
		r.emitEvent(st.strategy, pb.EventSeverity_EVENT_SEVERITY_ERROR, "StartFailed", err.Error())
		return
	}
	proc, err := r.deps.Driver.Start(sp, r.now())
	if err != nil {
		st.lastError = err.Error()
		st.phase = pb.DeployPhase_DEPLOY_PHASE_FAILED
		r.emitEvent(st.strategy, pb.EventSeverity_EVENT_SEVERITY_ERROR, "StartFailed", err.Error())
		return
	}
	r.installProcess(spec, st, proc)
	st.lastError = ""
	if healthCheck {
		st.phase = pb.DeployPhase_DEPLOY_PHASE_HEALTH_CHECKING
		st.healthDeadline = r.now().Add(healthWindow(spec))
	} else {
		st.phase = pb.DeployPhase_DEPLOY_PHASE_HEALTHY
		st.observedGen = r.generation
	}
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

func (r *Reconciler) buildStartSpec(spec *pb.StrategyAssignmentSpec, launch *pb.ArtifactRef) (driver.StartSpec, error) {
	if launch == nil {
		launch = spec.GetArtifact()
	}
	args, err := r.renderArgs(spec, launch)
	if err != nil {
		return driver.StartSpec{}, err
	}
	limits := spec.GetLimits()
	out := driver.StartSpec{
		Strategy:      spec.GetStrategy(),
		Args:          args,
		CPUMillicores: limits.GetCpuMillicores(),
		MemoryBytes:   limits.GetMemoryBytes(),
		MaxOpenFiles:  limits.GetMaxOpenFiles(),
	}
	if launch.GetType() == pb.ArtifactType_ARTIFACT_TYPE_OCI_IMAGE {
		return r.buildOCIStartSpec(spec, launch, out, args)
	}
	out.Driver = driver.KindExec
	out.BinaryPath = r.deps.Artifacts.CurrentBinaryPath(spec.GetStrategy())
	out.WorkDir = r.deps.Artifacts.StrategyDir(spec.GetStrategy())
	out.Env = envPairs(spec.GetEnv())
	return out, nil
}

func (r *Reconciler) buildOCIStartSpec(spec *pb.StrategyAssignmentSpec, launch *pb.ArtifactRef, out driver.StartSpec, renderedArgs []string) (driver.StartSpec, error) {
	strat := spec.GetStrategy()
	meta, err := artifact.ReadCurrentOCIMeta(r.deps.Artifacts, strat)
	if err != nil {
		return driver.StartSpec{}, fmt.Errorf("oci.json: %w", err)
	}
	rootfs := r.deps.Artifacts.CurrentRootfsPath(strat)
	if _, err := os.Stat(rootfs); err != nil {
		return driver.StartSpec{}, fmt.Errorf("oci rootfs missing: %w", err)
	}
	work, err := r.deps.Artifacts.EnsureWorkDir(strat)
	if err != nil {
		return driver.StartSpec{}, err
	}
	argv := append([]string(nil), meta.Entrypoint...)
	if len(renderedArgs) > 0 {
		argv = append(argv, renderedArgs...)
	} else {
		argv = append(argv, meta.Cmd...)
	}
	if len(argv) == 0 {
		return driver.StartSpec{}, fmt.Errorf("oci: empty entrypoint/cmd")
	}
	uid, gid := artifact.ParseUser(meta.User)
	out.Driver = driver.KindOCI
	out.Rootfs = rootfs
	out.Argv = argv
	out.ImageEnv = append([]string(nil), meta.Env...)
	out.Env = mergeEnv(meta.Env, spec.GetEnv())
	out.WorkDir = work
	out.WorkBind = work
	out.SharedBind = r.deps.Artifacts.SharedRoot()
	out.ContainerUID = uid
	out.ContainerGID = gid
	if cfg := spec.GetConfig(); cfg != nil && cfg.GetDigest() != "" {
		cfgPath, err := filepath.Abs(r.deps.Artifacts.CurrentConfigPath(strat, cfg))
		if err == nil {
			out.ConfigBind = cfgPath
		}
	}
	return out, nil
}

func envPairs(env map[string]string) []string {
	out := make([]string, 0, len(env))
	for k, v := range env {
		out = append(out, k+"="+v)
	}
	return out
}

// mergeEnv overlays spec env onto the image env. The result is never nil: a
// nil Env means "inherit the parent's environment" to exec.Cmd, which would
// hand the agent's own environment (control-plane URL, object-store
// credentials) to the strategy container.
func mergeEnv(image []string, spec map[string]string) []string {
	out := make([]string, 0, len(image)+len(spec))
	out = append(out, image...)
	for k, v := range spec {
		prefix := k + "="
		found := false
		for i, e := range out {
			if strings.HasPrefix(e, prefix) {
				out[i] = prefix + v
				found = true
				break
			}
		}
		if !found {
			out = append(out, prefix+v)
		}
	}
	return out
}

// renderArgs expands placeholders against the current symlink. OCI rejects
// ${RELEASE_DIR} and ${BINARY} — those paths are not bound into the container.
func (r *Reconciler) renderArgs(spec *pb.StrategyAssignmentSpec, launch *pb.ArtifactRef) ([]string, error) {
	raw := spec.GetArgs()
	if len(raw) == 0 {
		return nil, nil
	}
	oci := launch != nil && launch.GetType() == pb.ArtifactType_ARTIFACT_TYPE_OCI_IMAGE
	if oci {
		for _, arg := range raw {
			if strings.Contains(arg, "${RELEASE_DIR}") || strings.Contains(arg, "${BINARY}") {
				return nil, fmt.Errorf("OCI deployments cannot use ${RELEASE_DIR} or ${BINARY}")
			}
		}
	}
	vals, err := r.placeholderValues(spec, oci)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(raw))
	for i, arg := range raw {
		rendered, err := expandPlaceholders(arg, vals)
		if err != nil {
			return nil, err
		}
		out[i] = rendered
	}
	return out, nil
}

func (r *Reconciler) placeholderValues(spec *pb.StrategyAssignmentSpec, oci bool) (map[string]string, error) {
	strat := spec.GetStrategy()
	vals := map[string]string{}
	if !oci {
		releaseDir, err := r.deps.Artifacts.CurrentReleaseDir(strat)
		if err != nil {
			return nil, fmt.Errorf("resolve ${RELEASE_DIR}: %w", err)
		}
		binPath, err := filepath.Abs(r.deps.Artifacts.CurrentBinaryPath(strat))
		if err != nil {
			return nil, fmt.Errorf("resolve ${BINARY}: %w", err)
		}
		vals["RELEASE_DIR"] = releaseDir
		vals["BINARY"] = binPath
	}
	if cfg := spec.GetConfig(); cfg != nil && cfg.GetDigest() != "" {
		cfgPath, err := filepath.Abs(r.deps.Artifacts.CurrentConfigPath(strat, cfg))
		if err != nil {
			return nil, fmt.Errorf("resolve ${CONFIG}: %w", err)
		}
		vals["CONFIG"] = cfgPath
	}
	return vals, nil
}

func expandPlaceholders(arg string, vals map[string]string) (string, error) {
	var firstErr error
	out := placeholderRE.ReplaceAllStringFunc(arg, func(match string) string {
		if firstErr != nil {
			return match
		}
		name := match[2 : len(match)-1] // strip ${}
		v, ok := vals[name]
		if !ok {
			firstErr = fmt.Errorf("unknown placeholder ${%s}", name)
			return match
		}
		return v
	})
	if firstErr != nil {
		return "", firstErr
	}
	return out, nil
}

// handleExit processes a process-exit notification.
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
		r.recordStartCrash(st, ex.info)
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
		st.lastError = fmt.Sprintf("exited (code %d)", ex.info.Code)
	}
	if st.phase == pb.DeployPhase_DEPLOY_PHASE_HEALTHY {
		// HEALTHY means the desired version is running. A dead process must
		// not keep advertising that — crash-restart will promote again after
		// installProcess succeeds.
		st.phase = pb.DeployPhase_DEPLOY_PHASE_STARTING
	}
}

// recordStartCrash writes a current-generation lastError and, for OCI, a WARN
// pointing at the sibling oci-init.log when its first line is non-empty.
// An empty file is created on every successful Start, and WorkDir (plus the
// sibling log) survives redeploys, so existence alone is not a failure signal.
func (r *Reconciler) recordStartCrash(st *strategyState, info driver.ExitInfo) {
	st.lastError = fmt.Sprintf("exited after start (code %d)", info.Code)
	if r.deps.Artifacts == nil || !r.launchIsOCI(st) {
		return
	}
	logPath := driver.OCIInitLogPath(r.deps.Artifacts.WorkDir(st.strategy))
	b, err := os.ReadFile(logPath)
	if err != nil {
		return
	}
	line, _, _ := strings.Cut(string(b), "\n")
	line = strings.TrimSpace(line)
	if line == "" {
		return
	}
	r.logger().Warn("oci-init failed; see init log", "strategy", st.strategy, "path", logPath)
	st.lastError = line
}

func (r *Reconciler) launchIsOCI(st *strategyState) bool {
	if st.runningArtifact != nil {
		return st.runningArtifact.GetType() == pb.ArtifactType_ARTIFACT_TYPE_OCI_IMAGE
	}
	if spec := r.desired[st.strategy]; spec != nil {
		return spec.GetArtifact().GetType() == pb.ArtifactType_ARTIFACT_TYPE_OCI_IMAGE
	}
	return false
}

// tick drives time-based work: health-window evaluation, async readiness
// probing, and local cron schedule evaluation. Backoff
// wakeups are handled by reconcile() running after every tick.
func (r *Reconciler) tick(now time.Time) {
	for _, st := range r.actual {
		if st.phase != pb.DeployPhase_DEPLOY_PHASE_HEALTH_CHECKING || st.proc == nil {
			continue
		}
		if now.After(st.healthDeadline) {
			spec := r.desired[st.strategy]
			if hasReadinessProbe(spec) {
				r.emitEvent(st.strategy, pb.EventSeverity_EVENT_SEVERITY_ERROR, "HealthTimeout",
					"readiness not achieved within health window")
				if spec.GetDeployPolicy().GetEnableAutoRollback() && st.inflight != nil {
					r.beginRollback(spec, st)
				} else {
					r.failHealthWindow(st)
				}
				continue
			}
			if spec != nil && spec.GetDeployPolicy().GetEnableAutoRollback() && st.inflight != nil {
				r.emitEvent(st.strategy, pb.EventSeverity_EVENT_SEVERITY_ERROR, "HealthTimeout",
					"readiness not achieved within health window")
				r.beginRollback(spec, st)
			} else {
				// No probe: the window is a grace period. Unchanged for traders.
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
	if spec := r.desired[st.strategy]; spec != nil {
		endpoint = spec.GetReadiness().GetEndpoint()
	}
	if endpoint == "" && r.deps.ReadyEndpoint != nil {
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

func hasReadinessProbe(spec *pb.StrategyAssignmentSpec) bool {
	return spec != nil && spec.GetReadiness().GetEndpoint() != ""
}

func (r *Reconciler) failHealthWindow(st *strategyState) {
	st.phase = pb.DeployPhase_DEPLOY_PHASE_FAILED
	st.lastError = "readiness not achieved within health window"
	st.failedAtGen = r.generation
	if st.inflight != nil {
		st.inflight.cancel()
		st.inflight = nil
	}
	r.setCondition(st, conditionReady, pb.ConditionStatus_CONDITION_STATUS_FALSE, "HealthTimeout", st.lastError)
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
		if st == nil {
			converged = false
			break
		}
		if spec.GetStopped() {
			// Intentionally halted: settled once drained (no live process).
			if st.phase != pb.DeployPhase_DEPLOY_PHASE_STOPPED || st.proc != nil {
				converged = false
				break
			}
			continue
		}
		if !versionMatches(spec, st) || st.phase != pb.DeployPhase_DEPLOY_PHASE_HEALTHY || st.proc == nil {
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
	// supervision so the next agent can Adopt; strategies keep running.
	r.persistSupervision()
}

// versionMatches compares desired vs actual by content digest (artifact +
// config). Content addressing is the only trustworthy equality for "same
// bytes"; version/uri labels are promoted separately by promoteRunningLabels.
func versionMatches(spec *pb.StrategyAssignmentSpec, st *strategyState) bool {
	if st.runningArtifact == nil {
		return false
	}
	if st.runningArtifact.GetDigest() != spec.GetArtifact().GetDigest() {
		return false
	}
	return spec.GetConfig().GetDigest() == st.runningConfig.GetDigest()
}

// promoteRunningLabels copies desired artifact/config refs onto running when
// the digest already matches but version or uri differ (re-register of the
// same bytes under a new catalog name, typically http(s)/file → s3). No
// download, no process restart. Idempotent: a second tick is a no-op.
func (r *Reconciler) promoteRunningLabels(spec *pb.StrategyAssignmentSpec, st *strategyState) {
	if artifactLabelsEqual(spec.GetArtifact(), st.runningArtifact) &&
		artifactLabelsEqual(spec.GetConfig(), st.runningConfig) {
		return
	}
	oldVer := st.runningArtifact.GetVersion()
	oldURI := st.runningArtifact.GetUri()
	st.runningArtifact = cloneArtifactRef(spec.GetArtifact())
	st.runningConfig = cloneArtifactRef(spec.GetConfig())
	r.emitEvent(st.strategy, pb.EventSeverity_EVENT_SEVERITY_INFO, "VersionRelabeled",
		fmt.Sprintf("%s (%s) → %s (%s)", oldVer, oldURI, st.runningArtifact.GetVersion(), st.runningArtifact.GetUri()))
}

func artifactLabelsEqual(want, have *pb.ArtifactRef) bool {
	return want.GetVersion() == have.GetVersion() && want.GetUri() == have.GetUri()
}

func cloneArtifactRef(ref *pb.ArtifactRef) *pb.ArtifactRef {
	if ref == nil {
		return nil
	}
	return proto.Clone(ref).(*pb.ArtifactRef)
}

func healthWindow(spec *pb.StrategyAssignmentSpec) time.Duration {
	w := spec.GetDeployPolicy().GetHealthWindowSeconds()
	if w <= 0 {
		w = 30
	}
	return time.Duration(w) * time.Second
}
