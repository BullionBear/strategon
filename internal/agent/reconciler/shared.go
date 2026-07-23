package reconciler

import (
	"context"
	"fmt"
	"sort"
	"strings"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/agent/supervisor"
	"github.com/bullionbear/strategon/internal/sharedfile"
)

// sharedFileState tracks per-name actual shared-file convergence.
type sharedFileState struct {
	name          string
	runningDigest string
	lastError     string
	backoff       supervisor.BackoffState
	inflight      *sharedOp // non-nil = fetch worker in progress
}

// sharedOp tracks an in-flight shared-file fetch so it can be cancelled if
// desired changes. Mirrors deployOp.
type sharedOp struct {
	digest string
	cancel context.CancelFunc
}

// sharedWorkerEvent is emitted by a shared-file fetch worker. The main loop is
// the sole mutator of sharedActual; the worker only performs IO.
type sharedWorkerEvent struct {
	name   string
	digest string // desired digest this fetch was for
	err    error
}

func (r *Reconciler) applyDesiredShared(ds *pb.DesiredState) {
	shared := ds.GetShared()
	var next map[string]*pb.SharedFileSpec
	if shared == nil {
		next = map[string]*pb.SharedFileSpec{}
		r.sharedGeneration = 0
	} else {
		r.sharedGeneration = shared.GetGeneration()
		next = make(map[string]*pb.SharedFileSpec, len(shared.GetFiles()))
		for _, f := range shared.GetFiles() {
			if f == nil || f.GetName() == "" {
				continue
			}
			next[f.GetName()] = f
		}
	}
	// Cancel in-flight fetches whose target digest no longer matches (or whose
	// name was removed from desired).
	for name, st := range r.sharedActual {
		if st.inflight == nil {
			continue
		}
		spec, want := next[name]
		newDigest := ""
		if want && spec.GetArtifact() != nil {
			newDigest = spec.GetArtifact().GetDigest()
		}
		if !want || newDigest != st.inflight.digest {
			st.inflight.cancel()
			st.inflight = nil
		}
	}
	r.desiredShared = next
}

// reconcileShared converges machine-level shared files before assignments.
// Fetch/verify runs in a worker goroutine (same non-blocking pattern as deploy)
// so a hung artifact endpoint cannot stall the reconciler loop.
func (r *Reconciler) reconcileShared() {
	if r.deps.Artifacts == nil {
		return
	}
	// Drop symlinks for names no longer desired.
	for name, st := range r.sharedActual {
		if _, want := r.desiredShared[name]; want {
			continue
		}
		if st.inflight != nil {
			st.inflight.cancel()
			st.inflight = nil
		}
		if err := r.deps.Artifacts.RemoveSharedLink(name); err != nil {
			st.lastError = "remove failed: " + err.Error()
			// Keep the entry so buildSharedStatus can report the failure.
			continue
		}
		delete(r.sharedActual, name)
	}

	for name, spec := range r.desiredShared {
		st := r.sharedActual[name]
		if st == nil {
			st = &sharedFileState{name: name}
			r.sharedActual[name] = st
		}
		// Refresh running digest before backoff so status is not stale for the
		// whole backoff window (§3.4).
		running := r.deps.Artifacts.RunningSharedDigest(name)
		st.runningDigest = running

		if st.backoff.Blocked(r.now()) {
			continue
		}
		if st.inflight != nil {
			continue // fetch already in flight
		}
		if err := sharedfile.ValidateName(name); err != nil {
			st.lastError = err.Error()
			st.backoff.RecordCrash(r.now(), r.deps.Jitter)
			continue
		}
		want := ""
		if spec.GetArtifact() != nil {
			want = spec.GetArtifact().GetDigest()
		}
		if want != "" && digestsEqual(running, want) {
			st.lastError = ""
			st.backoff.Reset()
			continue
		}
		if want == "" {
			st.lastError = "empty desired digest"
			st.backoff.RecordCrash(r.now(), r.deps.Jitter)
			continue
		}
		r.beginSharedFetch(name, spec.GetArtifact(), st)
	}

	retention := r.deps.SharedRetention
	if retention <= 0 {
		retention = 3
	}
	keep := make(map[string]struct{}, len(r.desiredShared))
	for n := range r.desiredShared {
		keep[n] = struct{}{}
	}
	_ = r.deps.Artifacts.GCShared(retention, keep)
}

func (r *Reconciler) beginSharedFetch(name string, art *pb.ArtifactRef, st *sharedFileState) {
	ctx, cancel := context.WithCancel(r.ctx)
	st.inflight = &sharedOp{digest: art.GetDigest(), cancel: cancel}
	go r.runSharedFetch(ctx, name, art)
}

// runSharedFetch downloads+verifies a shared file. It never mutates reconciler
// state; SwitchSharedTo happens on the main loop via applySharedWorkerEvent.
func (r *Reconciler) runSharedFetch(ctx context.Context, name string, art *pb.ArtifactRef) {
	err := r.deps.Artifacts.EnsureSharedFile(ctx, name, art)
	select {
	case r.sharedCh <- sharedWorkerEvent{name: name, digest: art.GetDigest(), err: err}:
	case <-ctx.Done():
	}
}

func (r *Reconciler) applySharedWorkerEvent(ev sharedWorkerEvent) {
	st := r.sharedActual[ev.name]
	if st == nil || st.inflight == nil {
		return // cancelled / withdrawn
	}
	if ev.digest != st.inflight.digest {
		return // superseded
	}
	st.inflight = nil

	if ev.err != nil {
		st.lastError = ev.err.Error()
		st.backoff.RecordCrash(r.now(), r.deps.Jitter)
		r.emitEvent("", pb.EventSeverity_EVENT_SEVERITY_ERROR, "SharedFileError",
			fmt.Sprintf("%s: %v", ev.name, ev.err))
		return
	}
	if err := r.deps.Artifacts.SwitchSharedTo(ev.name, ev.digest); err != nil {
		st.lastError = err.Error()
		st.backoff.RecordCrash(r.now(), r.deps.Jitter)
		r.emitEvent("", pb.EventSeverity_EVENT_SEVERITY_ERROR, "SharedFileError",
			fmt.Sprintf("%s switch: %v", ev.name, err))
		return
	}
	st.runningDigest = r.deps.Artifacts.RunningSharedDigest(ev.name)
	st.lastError = ""
	st.backoff.Reset()
}

func digestsEqual(a, b string) bool {
	return a != "" && b != "" && strings.EqualFold(a, b)
}

func (r *Reconciler) buildSharedStatus() *pb.MachineSharedStatus {
	if r.sharedGeneration == 0 && len(r.desiredShared) == 0 && len(r.sharedActual) == 0 {
		return nil
	}
	// Union of desired and actual so failed removals (actual-only) are visible
	// to the control plane (§3.1).
	seen := map[string]struct{}{}
	names := make([]string, 0, len(r.desiredShared)+len(r.sharedActual))
	for n := range r.desiredShared {
		seen[n] = struct{}{}
		names = append(names, n)
	}
	for n := range r.sharedActual {
		if _, ok := seen[n]; ok {
			continue
		}
		names = append(names, n)
	}
	sort.Strings(names)
	files := make([]*pb.SharedFileStatus, 0, len(names))
	for _, n := range names {
		st := r.sharedActual[n]
		running := ""
		lastErr := ""
		if st != nil {
			running = st.runningDigest
			lastErr = st.lastError
		} else if r.deps.Artifacts != nil {
			running = r.deps.Artifacts.RunningSharedDigest(n)
		}
		files = append(files, &pb.SharedFileStatus{
			Name:          n,
			RunningDigest: running,
			LastError:     lastErr,
		})
	}
	return &pb.MachineSharedStatus{
		ObservedGeneration: r.sharedGeneration,
		Files:              files,
	}
}
