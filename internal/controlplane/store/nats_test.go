package store

import (
	"errors"
	"fmt"
	"sync"
	"testing"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

func TestMemoryNatsClusterUpsertAndGeneration(t *testing.T) {
	s := NewMemory(nil)
	in := &pb.NatsCluster{
		Metadata: &pb.ObjectMeta{Name: "trading", Labels: map[string]string{"env": "dev"}},
		Spec: &pb.NatsClusterSpec{
			ArtifactVersion: "v1",
			Strategy:        "nats",
			Servers:         []*pb.NatsServer{{Machine: "m1", ServerName: "n1", RouteHost: "10.0.0.1"}},
		},
	}
	out, changed, err := s.ApplyNatsCluster(in)
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
	again, changed, err := s.ApplyNatsCluster(&pb.NatsCluster{
		Metadata: &pb.ObjectMeta{Name: "trading", Labels: map[string]string{"env": "dev"}},
		Spec:     in.Spec,
	})
	if err != nil || changed || again.GetMetadata().GetGeneration() != 1 {
		t.Fatalf("identical: gen=%d changed=%v err=%v", again.GetMetadata().GetGeneration(), changed, err)
	}

	// Labels-only does not bump generation.
	labeled, changed, err := s.ApplyNatsCluster(&pb.NatsCluster{
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
	nextSpec := &pb.NatsClusterSpec{
		ArtifactVersion: "v2",
		Strategy:        "nats",
		Servers:         in.Spec.Servers,
	}
	bumped, changed, err := s.ApplyNatsCluster(&pb.NatsCluster{
		Metadata: &pb.ObjectMeta{Name: "trading", Labels: map[string]string{"env": "prod"}},
		Spec:     nextSpec,
	})
	if err != nil || !changed || bumped.GetMetadata().GetGeneration() != 2 {
		t.Fatalf("spec change: gen=%d changed=%v err=%v", bumped.GetMetadata().GetGeneration(), changed, err)
	}

	got, ok := s.GetNatsCluster("trading")
	if !ok || got.GetSpec().GetArtifactVersion() != "v2" {
		t.Fatalf("get = %+v ok=%v", got, ok)
	}
	list := s.ListNatsClusters()
	if len(list) != 1 || list[0].GetMetadata().GetName() != "trading" {
		t.Fatalf("list = %+v", list)
	}
}

func TestMemoryNatsClusterReservation(t *testing.T) {
	s := NewMemory(nil)
	_, _, err := s.ApplyNatsCluster(&pb.NatsCluster{
		Metadata: &pb.ObjectMeta{Name: "trading"},
		Spec: &pb.NatsClusterSpec{
			Strategy: "nats",
			Servers:  []*pb.NatsServer{{Machine: "m1", ServerName: "n1", RouteHost: "h"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	owner, ok := s.ReservedBy("m1", "nats")
	if !ok || owner != "trading" {
		t.Fatalf("reserved = %q ok=%v", owner, ok)
	}
	if _, ok := s.ReservedBy("m2", "nats"); ok {
		t.Fatal("m2 should not be reserved")
	}
	if _, err := s.MarkNatsClusterDeleting("trading"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.ReservedBy("m1", "nats"); ok {
		t.Fatal("deleting cluster should release reservation")
	}
	if err := s.DeleteNatsCluster("trading"); err != nil {
		t.Fatal(err)
	}
	if _, ok := s.GetNatsCluster("trading"); ok {
		t.Fatal("deleted cluster still present")
	}
}

func TestMemoryNatsClusterStatusPreservesDeleting(t *testing.T) {
	s := NewMemory(nil)
	if _, _, err := s.ApplyNatsCluster(&pb.NatsCluster{
		Metadata: &pb.ObjectMeta{Name: "trading"},
		Spec: &pb.NatsClusterSpec{
			Strategy: "nats",
			Servers:  []*pb.NatsServer{{Machine: "m1", ServerName: "n1", RouteHost: "h"}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := s.MarkNatsClusterDeleting("trading"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateNatsClusterStatus("trading", &pb.NatsClusterStatus{
		Phase:              "Ready",
		ObservedGeneration: 1,
		Servers:            []*pb.NatsServerStatus{{Machine: "m1", Ready: true}},
	}); err != nil {
		t.Fatal(err)
	}
	got, ok := s.GetNatsCluster("trading")
	if !ok {
		t.Fatal("missing cluster")
	}
	if !got.GetStatus().GetDeleting() || got.GetStatus().GetPhase() != "Deleting" {
		t.Fatalf("status = %+v, want Deleting preserved", got.GetStatus())
	}
}

// Concurrent applies claiming the same machine must not both land: the store
// re-checks ownership under its write lock, so exactly one wins no matter how
// the API-layer admission checks interleave.
func TestMemoryNatsClusterConcurrentApplyRejectsOverlap(t *testing.T) {
	s := NewMemory(nil)
	const n = 16
	var wg sync.WaitGroup
	errs := make([]error, n)
	start := make(chan struct{})
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			_, _, errs[i] = s.ApplyNatsCluster(&pb.NatsCluster{
				Metadata: &pb.ObjectMeta{Name: fmt.Sprintf("c%02d", i)},
				Spec: &pb.NatsClusterSpec{
					Strategy: "nats",
					Servers:  []*pb.NatsServer{{Machine: "m1", ServerName: "n1", RouteHost: "h"}},
				},
			})
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
	if got := len(s.ListNatsClusters()); got != 1 {
		t.Fatalf("stored clusters = %d, want 1", got)
	}
	owner, ok := s.ReservedBy("m1", "nats")
	if !ok {
		t.Fatal("m1 should be reserved by the winner")
	}
	if _, present := s.GetNatsCluster(owner); !present {
		t.Fatalf("reservation owner %q has no row", owner)
	}
}

// A cluster may still re-apply its own spec and grow onto free machines.
func TestMemoryNatsClusterReapplyAndGrowth(t *testing.T) {
	s := NewMemory(nil)
	spec := func(machines ...string) *pb.NatsClusterSpec {
		out := &pb.NatsClusterSpec{Strategy: "nats"}
		for _, m := range machines {
			out.Servers = append(out.Servers, &pb.NatsServer{Machine: m, ServerName: m, RouteHost: "h"})
		}
		return out
	}
	apply := func(name string, sp *pb.NatsClusterSpec) (*pb.NatsCluster, error) {
		out, _, err := s.ApplyNatsCluster(&pb.NatsCluster{
			Metadata: &pb.ObjectMeta{Name: name}, Spec: sp,
		})
		return out, err
	}
	if _, err := apply("trading", spec("m1")); err != nil {
		t.Fatal(err)
	}
	// Idempotent re-apply of an owned machine is not a self-conflict.
	if _, err := apply("trading", spec("m1")); err != nil {
		t.Fatalf("re-apply own spec: %v", err)
	}
	// Growing onto a free machine is allowed.
	out, err := apply("trading", spec("m1", "m2"))
	if err != nil {
		t.Fatalf("grow: %v", err)
	}
	if out.GetMetadata().GetGeneration() != 2 {
		t.Fatalf("generation = %d, want 2", out.GetMetadata().GetGeneration())
	}
	// A second cluster cannot take either machine.
	if _, err := apply("other", spec("m2")); err == nil {
		t.Fatal("overlapping apply should fail")
	}
}
