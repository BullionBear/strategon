package store

import (
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
