package reconciler

import (
	"os"
	"sort"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/volume"
)

func (r *Reconciler) applyDesiredVolumes(ds *pb.DesiredState) {
	if ds.GetVolumes() == nil {
		r.desiredVolumesPresent = false
		return
	}
	r.desiredVolumesPresent = true
	r.volumesGeneration = ds.GetVolumes().GetGeneration()
	next := map[string]struct{}{}
	for _, v := range ds.GetVolumes().GetVolumes() {
		if v == nil || v.GetName() == "" {
			continue
		}
		next[v.GetName()] = struct{}{}
	}
	r.desiredVolumes = next
}

func (r *Reconciler) reconcileVolumes() {
	if !r.desiredVolumesPresent {
		return
	}
	if r.deps.Artifacts == nil {
		return
	}
	for name := range r.desiredVolumes {
		if err := volume.ValidateName(name); err != nil {
			r.volumeErrors[name] = err.Error()
			continue
		}
		if _, err := r.deps.Artifacts.EnsureVolumeDir(name); err != nil {
			r.volumeErrors[name] = err.Error()
			continue
		}
		delete(r.volumeErrors, name)
	}
	entries, err := os.ReadDir(r.deps.Artifacts.VolumesRoot())
	if err != nil {
		if os.IsNotExist(err) {
			return
		}
		return
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if _, want := r.desiredVolumes[e.Name()]; want {
			continue
		}
		_ = os.RemoveAll(r.deps.Artifacts.VolumeDir(e.Name()))
		delete(r.volumeErrors, e.Name())
	}
}

func (r *Reconciler) volumePresent(mounts []*pb.VolumeMount) bool {
	if len(mounts) == 0 {
		return true
	}
	if r.deps.Artifacts == nil {
		return false
	}
	for _, m := range mounts {
		st, err := os.Stat(r.deps.Artifacts.VolumeDir(m.GetName()))
		if err != nil || !st.IsDir() {
			return false
		}
	}
	return true
}

func (r *Reconciler) awaitVolumeReady(st *strategyState, spec *pb.StrategyAssignmentSpec) bool {
	if r.volumePresent(spec.GetVolumeMounts()) {
		return true
	}
	if st.warnedWaitingVolume != r.volumesGeneration {
		st.warnedWaitingVolume = r.volumesGeneration
		r.emitEvent(st.strategy, pb.EventSeverity_EVENT_SEVERITY_WARNING, "WaitingForVolume",
			"deferring start until mounted volumes are present on disk")
	}
	return false
}

func (r *Reconciler) volumeWriterConflict(self string, mounts []*pb.VolumeMount) string {
	want := map[string]struct{}{}
	for _, m := range mounts {
		want[m.GetName()] = struct{}{}
	}
	if len(want) == 0 {
		return ""
	}
	for name, st := range r.actual {
		if name == self || st.proc == nil {
			continue
		}
		spec := r.desired[name]
		if spec == nil {
			continue
		}
		for _, m := range spec.GetVolumeMounts() {
			if _, ok := want[m.GetName()]; ok {
				return m.GetName()
			}
		}
	}
	return ""
}

func (r *Reconciler) buildVolumeStatus() *pb.MachineVolumeStatus {
	if !r.desiredVolumesPresent && len(r.volumeErrors) == 0 {
		return nil
	}
	names := make([]string, 0, len(r.desiredVolumes))
	for n := range r.desiredVolumes {
		names = append(names, n)
	}
	sort.Strings(names)
	mounted := r.runningVolumeMounts()
	out := make([]*pb.VolumeStatus, 0, len(names))
	for _, n := range names {
		st := &pb.VolumeStatus{Name: n, MountedBy: mounted[n]}
		if r.deps.Artifacts != nil {
			if info, err := os.Stat(r.deps.Artifacts.VolumeDir(n)); err == nil && info.IsDir() {
				st.Ready = true
			}
		}
		if err := r.volumeErrors[n]; err != "" {
			st.LastError = err
			st.Ready = false
		}
		out = append(out, st)
	}
	return &pb.MachineVolumeStatus{
		ObservedGeneration: r.volumesGeneration,
		Volumes:            out,
	}
}

func (r *Reconciler) runningVolumeMounts() map[string][]string {
	out := map[string][]string{}
	for name, st := range r.actual {
		if st.proc == nil {
			continue
		}
		spec := r.desired[name]
		if spec == nil {
			continue
		}
		for _, m := range spec.GetVolumeMounts() {
			out[m.GetName()] = append(out[m.GetName()], name)
		}
	}
	for _, names := range out {
		sort.Strings(names)
	}
	return out
}
