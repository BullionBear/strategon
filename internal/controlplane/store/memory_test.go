package store

import (
	"testing"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

func TestGenerationMonotonicAndDesiredSnapshot(t *testing.T) {
	s := NewMemory(nil)
	if _, err := s.UpsertMachine(&pb.Register{MachineId: "m1", AgentVersion: 1}); err != nil {
		t.Fatal(err)
	}

	ds, ok := s.DesiredState("m1")
	if !ok || ds.GetGeneration() != 0 || len(ds.GetAssignments()) != 0 {
		t.Fatalf("fresh machine should have generation 0 and no assignments")
	}

	g1, err := s.SetAssignment("m1", "s", &pb.StrategyAssignmentSpec{Strategy: "s", Artifact: &pb.ArtifactRef{Version: "v1"}})
	if err != nil {
		t.Fatal(err)
	}
	g2, _ := s.SetAssignment("m1", "s", &pb.StrategyAssignmentSpec{Strategy: "s", Artifact: &pb.ArtifactRef{Version: "v2"}})
	if !(g2 > g1) {
		t.Fatalf("generation must increase monotonically: g1=%d g2=%d", g1, g2)
	}

	ds, _ = s.DesiredState("m1")
	if ds.GetGeneration() != g2 || len(ds.GetAssignments()) != 1 {
		t.Fatalf("desired snapshot mismatch: gen=%d n=%d", ds.GetGeneration(), len(ds.GetAssignments()))
	}
	if ds.GetAssignments()[0].GetArtifact().GetVersion() != "v2" {
		t.Fatalf("expected latest v2 spec")
	}

	// Removing an assignment also bumps generation.
	g3, _ := s.SetAssignment("m1", "s", nil)
	if g3 <= g2 {
		t.Fatalf("removal must bump generation")
	}
	ds, _ = s.DesiredState("m1")
	if len(ds.GetAssignments()) != 0 {
		t.Fatalf("assignment should be removed")
	}
}

func TestApplyStatusAndHeartbeat(t *testing.T) {
	s := NewMemory(nil)
	s.UpsertMachine(&pb.Register{MachineId: "m1"})
	if err := s.ApplyStatus("m1", &pb.StatusReport{ObservedGeneration: 5}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyHeartbeat("m1", &pb.Heartbeat{ObservedGeneration: 7, AgentVersion: 3}, 100); err != nil {
		t.Fatal(err)
	}
	rec, _ := s.GetMachine("m1")
	if rec.ObservedGen != 7 || rec.AgentVersion != 3 || rec.LastHeartbeat != 100 {
		t.Fatalf("unexpected record: %+v", rec)
	}
}
