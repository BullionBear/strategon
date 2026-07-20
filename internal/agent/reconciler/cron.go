package reconciler

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	agentcron "github.com/bullionbear/strategon/internal/agent/cron"
)

// cronEntry tracks the next due time for one named schedule on a strategy.
type cronEntry struct {
	nextFire time.Time
	// fingerprint of name|expr|tz|action|jitter|script so desired changes re-prime.
	key string
	// deferredLogged avoids spamming CronDeferred every tick while blocked.
	deferredLogged bool
}

func scheduleKey(s *pb.CronSchedule) string {
	return fmt.Sprintf("%s|%s|%s|%d|%d|%s",
		s.GetName(), s.GetCronExpr(), s.GetTimezone(),
		s.GetAction(), s.GetJitterSeconds(), s.GetScriptRef())
}

// tickCron evaluates due schedules for all strategies.
// When a deploy is in flight (or the strategy is not HEALTHY), due actions are
// deferred until the next eligible tick.
func (r *Reconciler) tickCron(now time.Time) {
	for name, spec := range r.desired {
		st := r.actual[name]
		if st == nil {
			continue
		}
		r.evalSchedules(spec, st, now)
	}
}

func (r *Reconciler) evalSchedules(spec *pb.StrategyAssignmentSpec, st *strategyState, now time.Time) {
	schedules := spec.GetSchedules()
	if len(schedules) == 0 {
		st.cron = nil
		return
	}
	if st.cron == nil {
		st.cron = map[string]*cronEntry{}
	}
	// Drop entries for removed schedule names.
	live := map[string]struct{}{}
	for _, s := range schedules {
		n := s.GetName()
		if n == "" {
			n = scheduleKey(s)
		}
		live[n] = struct{}{}
		r.ensureCronEntry(st, n, s, now)
		ent := st.cron[n]
		if ent == nil || now.Before(ent.nextFire) {
			continue
		}
		if !r.cronCanRun(st) {
			if !ent.deferredLogged {
				ent.deferredLogged = true
				r.emitEvent(st.strategy, pb.EventSeverity_EVENT_SEVERITY_INFO, "CronDeferred",
					fmt.Sprintf("schedule %q due but deploy/restart in progress (phase=%s)", n, st.phase))
			}
			continue
		}
		ent.deferredLogged = false
		if err := r.dispatchCron(spec, st, s); err != nil {
			r.emitEvent(st.strategy, pb.EventSeverity_EVENT_SEVERITY_ERROR, "CronFailed",
				fmt.Sprintf("schedule %q: %v", n, err))
		} else {
			r.emitEvent(st.strategy, pb.EventSeverity_EVENT_SEVERITY_INFO, "CronExecuted",
				fmt.Sprintf("schedule %q action=%s", n, s.GetAction().String()))
		}
		// Advance even on failure so a bad action cannot hot-loop every tick.
		parsed, err := agentcron.Parse(s.GetCronExpr(), s.GetTimezone())
		if err != nil {
			ent.nextFire = now.Add(time.Hour) // back off; SetSchedule should have validated
			continue
		}
		ent.nextFire = parsed.NextAfter(now, s.GetJitterSeconds(), r.deps.CronRand)
	}
	for n := range st.cron {
		if _, ok := live[n]; !ok {
			delete(st.cron, n)
		}
	}
}

func (r *Reconciler) ensureCronEntry(st *strategyState, name string, s *pb.CronSchedule, now time.Time) {
	key := scheduleKey(s)
	ent := st.cron[name]
	if ent != nil && ent.key == key && !ent.nextFire.IsZero() {
		return
	}
	parsed, err := agentcron.Parse(s.GetCronExpr(), s.GetTimezone())
	if err != nil {
		r.emitEvent(st.strategy, pb.EventSeverity_EVENT_SEVERITY_ERROR, "CronInvalid",
			fmt.Sprintf("schedule %q: %v", name, err))
		st.cron[name] = &cronEntry{key: key, nextFire: now.Add(24 * time.Hour)}
		return
	}
	st.cron[name] = &cronEntry{
		key:      key,
		nextFire: parsed.NextAfter(now, s.GetJitterSeconds(), r.deps.CronRand),
	}
}

// cronCanRun is true when a cron action may mutate process state.
// Inflight deploys and non-steady phases defer.
func (r *Reconciler) cronCanRun(st *strategyState) bool {
	if st.inflight != nil || st.stopping {
		return false
	}
	return st.phase == pb.DeployPhase_DEPLOY_PHASE_HEALTHY && st.proc != nil
}

func (r *Reconciler) dispatchCron(spec *pb.StrategyAssignmentSpec, st *strategyState, s *pb.CronSchedule) error {
	switch s.GetAction() {
	case pb.CronAction_CRON_ACTION_RESTART:
		r.spawnDrain(st, spec, false)
		return nil
	case pb.CronAction_CRON_ACTION_RELOAD_CONFIG:
		if st.proc == nil {
			return fmt.Errorf("no process to signal")
		}
		return r.deps.Driver.Signal(st.proc, syscall.SIGHUP)
	case pb.CronAction_CRON_ACTION_RUN_SCRIPT:
		return r.runCronScript(spec, s)
	default:
		return fmt.Errorf("unsupported action %v", s.GetAction())
	}
}

// runCronScript executes script_ref relative to the strategy directory (or as an
// absolute path). It runs asynchronously so the reconciler loop stays non-blocking.
func (r *Reconciler) runCronScript(spec *pb.StrategyAssignmentSpec, s *pb.CronSchedule) error {
	ref := s.GetScriptRef()
	if ref == "" {
		return fmt.Errorf("script_ref required for RUN_SCRIPT")
	}
	path := ref
	if !filepath.IsAbs(path) {
		path = filepath.Join(r.deps.Artifacts.StrategyDir(spec.GetStrategy()), ref)
	}
	if _, err := os.Stat(path); err != nil {
		return fmt.Errorf("script %q: %w", path, err)
	}
	strategy := spec.GetStrategy()
	name := s.GetName()
	go func() {
		cmd := exec.CommandContext(r.ctx, path)
		cmd.Dir = r.deps.Artifacts.StrategyDir(strategy)
		out, err := cmd.CombinedOutput()
		msg := string(out)
		if len(msg) > 512 {
			msg = msg[:512] + "…"
		}
		sev := pb.EventSeverity_EVENT_SEVERITY_INFO
		reason := "CronScriptFinished"
		if err != nil {
			sev = pb.EventSeverity_EVENT_SEVERITY_ERROR
			reason = "CronScriptFailed"
			if msg == "" {
				msg = err.Error()
			} else {
				msg = err.Error() + ": " + msg
			}
		}
		// Re-enter via workerCh-shaped path: emit from a tiny helper that only
		// sends an event (safe: send is non-blocking).
		r.emitEvent(strategy, sev, reason, fmt.Sprintf("schedule %q: %s", name, msg))
	}()
	return nil
}
