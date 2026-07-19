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

func TestRegisterArtifactRejectsRelativeFileURI(t *testing.T) {
	s := NewMemory(nil)
	err := s.RegisterArtifact(&pb.ArtifactRef{
		Name: "s", Version: "v1", Digest: "sha256:aaa", Uri: "file://tmp/myapp/strat-v1.sh",
	})
	if err == nil {
		t.Fatal("expected error for file://tmp/... (two slashes)")
	}
	if err := s.RegisterArtifact(&pb.ArtifactRef{
		Name: "s", Version: "v1", Digest: "sha256:aaa", Uri: "file:///tmp/myapp/strat-v1.sh",
	}); err != nil {
		t.Fatalf("absolute file URI should be accepted: %v", err)
	}
	if err := s.RegisterArtifact(&pb.ArtifactRef{
		Name: "s", Version: "v2", Digest: "sha256:bbb",
		Uri: "https://github.com/org/repo/releases/download/v2/strat",
	}); err != nil {
		t.Fatalf("https URI should be accepted: %v", err)
	}
}

func TestApplyStatusAndHeartbeat(t *testing.T) {
	s := NewMemory(nil)
	s.UpsertMachine(&pb.Register{MachineId: "m1", AgentBuildVersion: "v1.0.0"})
	if err := s.ApplyStatus("m1", &pb.StatusReport{ObservedGeneration: 5}); err != nil {
		t.Fatal(err)
	}
	if err := s.ApplyHeartbeat("m1", &pb.Heartbeat{
		ObservedGeneration: 7, AgentVersion: 3, AgentBuildVersion: "v1.0.1-dirty",
	}, 100); err != nil {
		t.Fatal(err)
	}
	rec, _ := s.GetMachine("m1")
	if rec.ObservedGen != 7 || rec.AgentVersion != 3 || rec.LastHeartbeat != 100 ||
		rec.AgentBuildVersion != "v1.0.1-dirty" {
		t.Fatalf("unexpected record: %+v", rec)
	}
}

func TestUpsertMachineStoresBuildVersion(t *testing.T) {
	s := NewMemory(nil)
	if _, err := s.UpsertMachine(&pb.Register{
		MachineId: "m1", AgentVersion: 2, AgentBuildVersion: "v1.4.2-3-gabc1234",
	}); err != nil {
		t.Fatal(err)
	}
	rec, ok := s.GetMachine("m1")
	if !ok || rec.AgentVersion != 2 || rec.AgentBuildVersion != "v1.4.2-3-gabc1234" {
		t.Fatalf("unexpected record: ok=%v %+v", ok, rec)
	}
}
