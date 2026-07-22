package reconciler

import (
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
}

func (r *Reconciler) applyDesiredShared(ds *pb.DesiredState) {
	shared := ds.GetShared()
	if shared == nil {
		r.desiredShared = map[string]*pb.SharedFileSpec{}
		r.sharedGeneration = 0
		return
	}
	r.sharedGeneration = shared.GetGeneration()
	next := make(map[string]*pb.SharedFileSpec, len(shared.GetFiles()))
	for _, f := range shared.GetFiles() {
		if f == nil || f.GetName() == "" {
			continue
		}
		next[f.GetName()] = f
	}
	r.desiredShared = next
}

// reconcileShared converges machine-level shared files before assignments.
// Fetch/verify is synchronous (shared files are small reference data); failures
// back off without blocking assignment converge forever.
func (r *Reconciler) reconcileShared() {
	if r.deps.Artifacts == nil {
		return
	}
	// Drop symlinks for names no longer desired.
	for name, st := range r.sharedActual {
		if _, want := r.desiredShared[name]; want {
			continue
		}
		if err := r.deps.Artifacts.RemoveSharedLink(name); err != nil {
			st.lastError = err.Error()
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
		if st.backoff.Blocked(r.now()) {
			continue
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
		running := r.deps.Artifacts.RunningSharedDigest(name)
		st.runningDigest = running
		if want != "" && digestsEqual(running, want) {
			st.lastError = ""
			st.backoff.Reset()
			continue
		}
		if want == "" {
			st.lastError = "empty desired digest"
			continue
		}
		if err := r.deps.Artifacts.EnsureSharedFile(r.ctx, name, spec.GetArtifact()); err != nil {
			st.lastError = err.Error()
			st.backoff.RecordCrash(r.now(), r.deps.Jitter)
			r.emitEvent("", pb.EventSeverity_EVENT_SEVERITY_ERROR, "SharedFileError",
				fmt.Sprintf("%s: %v", name, err))
			continue
		}
		if err := r.deps.Artifacts.SwitchSharedTo(name, want); err != nil {
			st.lastError = err.Error()
			st.backoff.RecordCrash(r.now(), r.deps.Jitter)
			r.emitEvent("", pb.EventSeverity_EVENT_SEVERITY_ERROR, "SharedFileError",
				fmt.Sprintf("%s switch: %v", name, err))
			continue
		}
		st.runningDigest = r.deps.Artifacts.RunningSharedDigest(name)
		st.lastError = ""
		st.backoff.Reset()
	}

	retention := r.deps.SharedRetention
	if retention <= 0 {
		retention = 3
	}
	_ = r.deps.Artifacts.GCShared(retention)
}

func digestsEqual(a, b string) bool {
	return a != "" && b != "" && strings.EqualFold(a, b)
}

func (r *Reconciler) buildSharedStatus() *pb.MachineSharedStatus {
	if r.sharedGeneration == 0 && len(r.desiredShared) == 0 && len(r.sharedActual) == 0 {
		return nil
	}
	names := make([]string, 0, len(r.desiredShared))
	for n := range r.desiredShared {
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
