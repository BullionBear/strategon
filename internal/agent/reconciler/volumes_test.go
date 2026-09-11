package reconciler

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

func TestReconcileVolumesNilDoesNotGC(t *testing.T) {
	r, _, mgr, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	stray := filepath.Join(mgr.VolumesRoot(), "stray")
	if err := os.MkdirAll(stray, 0o700); err != nil {
		t.Fatal(err)
	}
	r.applyDesiredVolumes(&pb.DesiredState{})
	r.reconcileVolumes()
	if _, err := os.Stat(stray); err != nil {
		t.Fatalf("nil DesiredState.volumes must not GC: %v", err)
	}

	r.applyDesiredVolumes(&pb.DesiredState{Volumes: &pb.MachineVolumeSpec{Generation: 1}})
	r.reconcileVolumes()
	if _, err := os.Stat(stray); !os.IsNotExist(err) {
		t.Fatalf("empty non-nil volumes must GC stray, err=%v", err)
	}
}

func TestReconcileVolumesCreatesDesired(t *testing.T) {
	r, _, mgr, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	r.applyDesiredVolumes(&pb.DesiredState{
		Volumes: &pb.MachineVolumeSpec{
			Generation: 2,
			Volumes:    []*pb.VolumeSpec{{Name: "data"}},
		},
	})
	r.reconcileVolumes()
	st, err := os.Stat(mgr.VolumeDir("data"))
	if err != nil || !st.IsDir() {
		t.Fatalf("desired volume missing: %v", err)
	}
	st2 := newStrategyState("s")
	spec := &pb.StrategyAssignmentSpec{
		VolumeMounts: []*pb.VolumeMount{{Name: "data", ContainerPath: "/data"}},
	}
	if !r.awaitVolumeReady(st2, spec) {
		t.Fatal("volume should be ready")
	}
}

func TestVolumeWriterConflict(t *testing.T) {
	r, fd, _, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	r.desired["a"] = &pb.StrategyAssignmentSpec{
		Strategy:     "a",
		VolumeMounts: []*pb.VolumeMount{{Name: "data", ContainerPath: "/data"}},
	}
	st := newStrategyState("a")
	st.proc = mustStart(t, fd)
	r.actual["a"] = st
	if got := r.volumeWriterConflict("b", []*pb.VolumeMount{{Name: "data", ContainerPath: "/x"}}); got != "data" {
		t.Fatalf("conflict = %q", got)
	}
	if got := r.volumeWriterConflict("a", []*pb.VolumeMount{{Name: "data", ContainerPath: "/data"}}); got != "" {
		t.Fatalf("self should not conflict: %q", got)
	}
}
