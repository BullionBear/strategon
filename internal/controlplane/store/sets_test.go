package store

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

func TestMemoryAssignmentSetUpsertAndGeneration(t *testing.T) {
	s := NewMemory(nil)
	in := &pb.AssignmentSet{
		Metadata: &pb.ObjectMeta{Name: "trading", Labels: map[string]string{"env": "dev"}},
		Spec: &pb.AssignmentSetSpec{
			ArtifactVersion: "v1",
			Strategy:        "nats",
			Members:         []*pb.SetMember{{Machine: "m1", Name: "n1", Vars: map[string]string{"route_host": "10.0.0.1"}}},
		},
	}
	out, changed, err := s.ApplyAssignmentSet(in)
	if err != nil || !changed {
		t.Fatalf("create: changed=%v err=%v", changed, err)
	}
	if out.GetMetadata().GetUid() == "" || out.GetMetadata().GetGeneration() != 1 || out.GetMetadata().GetCreatedAt() == nil {
		t.Fatalf("meta = %+v", out.GetMetadata())
	}
	if out.GetStatus().GetPhase() != "Pending" || out.GetStatus().GetObservedGeneration() != 0 {
		t.Fatalf("status = %+v", out.GetStatus())
	}

	// Identical re-apply is a no-op.
	again, changed, err := s.ApplyAssignmentSet(&pb.AssignmentSet{
		Metadata: &pb.ObjectMeta{Name: "trading", Labels: map[string]string{"env": "dev"}},
		Spec:     in.Spec,
	})
	if err != nil || changed || again.GetMetadata().GetGeneration() != 1 {
		t.Fatalf("identical: gen=%d changed=%v err=%v", again.GetMetadata().GetGeneration(), changed, err)
	}

	// Labels-only does not bump generation.
	labeled, changed, err := s.ApplyAssignmentSet(&pb.AssignmentSet{
		Metadata: &pb.ObjectMeta{Name: "trading", Labels: map[string]string{"env": "prod"}},
		Spec:     in.Spec,
	})
	if err != nil || !changed || labeled.GetMetadata().GetGeneration() != 1 {
		t.Fatalf("labels: gen=%d changed=%v err=%v", labeled.GetMetadata().GetGeneration(), changed, err)
	}
	if labeled.GetMetadata().GetLabels()["env"] != "prod" {
		t.Fatalf("labels = %v", labeled.GetMetadata().GetLabels())
	}

	// Spec change bumps generation.
	nextSpec := &pb.AssignmentSetSpec{
		ArtifactVersion: "v2",
		Strategy:        "nats",
		Members:         in.Spec.Members,
	}
	bumped, changed, err := s.ApplyAssignmentSet(&pb.AssignmentSet{
		Metadata: &pb.ObjectMeta{Name: "trading", Labels: map[string]string{"env": "prod"}},
		Spec:     nextSpec,
	})
	if err != nil || !changed || bumped.GetMetadata().GetGeneration() != 2 {
		t.Fatalf("spec change: gen=%d changed=%v err=%v", bumped.GetMetadata().GetGeneration(), changed, err)
	}

	got, ok := s.GetAssignmentSet("trading")
	if !ok || got.GetSpec().GetArtifactVersion() != "v2" {
		t.Fatalf("get = %+v ok=%v", got, ok)
	}
	list := s.ListAssignmentSets()
	if len(list) != 1 || list[0].GetMetadata().GetName() != "trading" {
		t.Fatalf("list = %+v", list)
	}
}

func TestMemoryAssignmentSetReservation(t *testing.T) {
	s := NewMemory(nil)
	_, _, err := s.ApplyAssignmentSet(&pb.AssignmentSet{
		Metadata: &pb.ObjectMeta{Name: "trading"},
		Spec: &pb.AssignmentSetSpec{
			Strategy: "nats",
			Members:  []*pb.SetMember{{Machine: "m1", Name: "n1", Vars: map[string]string{"route_host": "h"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	owner, ok := s.ReservedBy("m1", "nats")
	if !ok || owner != "trading" {
		t.Fatalf("reserved family = %q ok=%v", owner, ok)
	}
	owner, ok = s.ReservedBy("m1", "n1")
	if !ok || owner != "trading" {
		t.Fatalf("reserved member = %q ok=%v", owner, ok)
	}
	if slots := s.ReservedSlots("m1"); len(slots) != 2 {
		t.Fatalf("ReservedSlots = %v", slots)
	}
	if _, ok := s.ReservedBy("m2", "nats"); ok {
		t.Fatal("m2 should not be reserved")
	}
	if _, err := s.MarkAssignmentSetDeleting("trading"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.ReservedBy("m1", "nats"); ok {
		t.Fatal("deleting cluster should release reservation")
	}
	if err := s.DeleteAssignmentSet("trading"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.GetAssignmentSet("trading"); ok {
		t.Fatal("deleted cluster still present")
	}
}

func TestMemoryAssignmentSetStatusPreservesDeleting(t *testing.T) {
	s := NewMemory(nil)
	if _, _, err := s.ApplyAssignmentSet(&pb.AssignmentSet{
		Metadata: &pb.ObjectMeta{Name: "trading"},
		Spec: &pb.AssignmentSetSpec{
			Strategy: "nats",
			Members:  []*pb.SetMember{{Machine: "m1", Name: "n1", Vars: map[string]string{"route_host": "h"}}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkAssignmentSetDeleting("trading"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateAssignmentSetStatus("trading", &pb.AssignmentSetStatus{
		Phase:              "Ready",
		ObservedGeneration: 1,
		Members:            []*pb.MemberStatus{{Machine: "m1", Ready: true}},
	}); err != nil {
		t.Fatal(err)
	}
	got, ok := s.GetAssignmentSet("trading")
	if !ok {
		t.Fatal("missing cluster")
	}
	if !got.GetStatus().GetDeleting() || got.GetStatus().GetPhase() != "Deleting" {
		t.Fatalf("status = %+v, want Deleting preserved", got.GetStatus())
	}
}

func setSpec(machines ...string) *pb.AssignmentSetSpec {
	out := &pb.AssignmentSetSpec{Strategy: "nats"}
	for _, m := range machines {
		out.Members = append(out.Members, &pb.SetMember{Machine: m, Name: m, Vars: map[string]string{"route_host": "h"}})
	}
	return out
}

func applySet(s Store, name string, spec *pb.AssignmentSetSpec) (*pb.AssignmentSet, error) {
	out, _, err := s.ApplyAssignmentSet(&pb.AssignmentSet{
		Metadata: &pb.ObjectMeta{Name: name}, Spec: spec,
	})
	return out, err
}

// Concurrent applies claiming the same machine must not both land: the store
// re-checks ownership under its write lock, so exactly one wins no matter how
// the API-layer admission checks interleave.
func assertConcurrentOverlapRejected(t *testing.T, s Store) {
	t.Helper()
	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, errs[i] = applySet(s, fmt.Sprintf("c%02d", i), setSpec("m1"))
		}(i)
	}
	close(start)
	wg.Wait()

	won := 0
	for i, err := range errs {
		if err == nil {
			won++
			continue
		}
		var conflict *ReservationConflictError
		if !errors.As(err, &conflict) {
			t.Fatalf("apply %d: unexpected error %v", i, err)
		}
	}
	if won != 1 {
		t.Fatalf("concurrent applies for m1: %d winners, want 1", won)
	}
	if got := len(s.ListAssignmentSets()); got != 1 {
		t.Fatalf("stored clusters = %d, want 1", got)
	}
	owner, ok := s.ReservedBy("m1", "nats")
	if !ok {
		t.Fatal("m1 should be reserved by the winner")
	}
	if _, present := s.GetAssignmentSet(owner); !present {
		t.Fatalf("reservation owner %q has no row", owner)
	}
}

func assertReapplyAndGrowth(t *testing.T, s Store) {
	t.Helper()
	if _, err := applySet(s, "trading", setSpec("m1")); err != nil {
		t.Fatal(err)
	}
	if _, err := applySet(s, "trading", setSpec("m1")); err != nil {
		t.Fatalf("re-apply own spec: %v", err)
	}
	out, err := applySet(s, "trading", setSpec("m1", "m2"))
	if err != nil {
		t.Fatalf("grow: %v", err)
	}
	if out.GetMetadata().GetGeneration() != 2 {
		t.Fatalf("generation = %d, want 2", out.GetMetadata().GetGeneration())
	}
	if _, err := applySet(s, "other", setSpec("m2")); err == nil {
		t.Fatal("overlapping apply should fail")
	}
}

func TestMemoryAssignmentSetConcurrentApplyRejectsOverlap(t *testing.T) {
	assertConcurrentOverlapRejected(t, NewMemory(nil))
}

func TestMemoryAssignmentSetReapplyAndGrowth(t *testing.T) {
	assertReapplyAndGrowth(t, NewMemory(nil))
}

func TestMemoryAssignmentSetSameMachineDifferentNamesAfterFlip(t *testing.T) {
	s := NewMemory(nil)
	if _, err := applySet(s, "alpha", &pb.AssignmentSetSpec{
		Strategy: "nats",
		Members:  []*pb.SetMember{{Machine: "m1", Name: "n1", Vars: map[string]string{"route_host": "h"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := applySet(s, "beta", &pb.AssignmentSetSpec{
		Strategy: "nats",
		Members:  []*pb.SetMember{{Machine: "m1", Name: "n2", Vars: map[string]string{"route_host": "h"}}},
	}); err == nil {
		t.Fatal("two new sets must still conflict on the family name")
	}
	if err := s.UpdateAssignmentSetStatus("alpha", &pb.AssignmentSetStatus{
		Phase: "Ready", AssignmentKey: AssignmentKeyMember,
	}); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.ReservedBy("m1", "nats"); ok {
		t.Fatal("family name must be free after assignment_key=member")
	}
	if _, err := applySet(s, "beta", &pb.AssignmentSetSpec{
		Strategy: "nats",
		Members:  []*pb.SetMember{{Machine: "m1", Name: "n2", Vars: map[string]string{"route_host": "h"}}},
	}); err != nil {
		t.Fatalf("same machine, different member names after flip: %v", err)
	}
}

func TestMemoryAssignmentSetCrossCatalogSameMemberName(t *testing.T) {
	s := NewMemory(nil)
	if _, err := applySet(s, "alpha", &pb.AssignmentSetSpec{
		Strategy: "nats",
		Members:  []*pb.SetMember{{Machine: "m1", Name: "foo", Vars: map[string]string{"route_host": "h"}}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := applySet(s, "beta", &pb.AssignmentSetSpec{
		Strategy: "feed",
		Members:  []*pb.SetMember{{Machine: "m1", Name: "foo", Vars: map[string]string{"route_host": "h"}}},
	}); err == nil {
		t.Fatal("same member name on the same machine must conflict across catalogs")
	}
}
