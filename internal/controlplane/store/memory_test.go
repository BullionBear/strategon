package store

import (
	"testing"
	"time"

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

func TestRegisterArtifactSetsCreatedAtAndListsNewestFirst(t *testing.T) {
	s := NewMemory(nil)
	base := time.Date(2026, 7, 19, 12, 0, 0, 0, time.UTC)
	n := 0
	s.SetClock(func() time.Time {
		n++
		return base.Add(time.Duration(n) * time.Minute)
	})

	if err := s.RegisterArtifact(&pb.ArtifactRef{
		Name: "s", Version: "v1", Digest: "sha256:aaa", Uri: "file:///tmp/a",
	}); err != nil {
		t.Fatal(err)
	}
	if err := s.RegisterArtifact(&pb.ArtifactRef{
		Name: "s", Version: "v2", Digest: "sha256:bbb", Uri: "file:///tmp/b",
	}); err != nil {
		t.Fatal(err)
	}

	list := s.ListArtifacts("s")
	if len(list) != 2 {
		t.Fatalf("len = %d", len(list))
	}
	if list[0].GetVersion() != "v2" || list[1].GetVersion() != "v1" {
		t.Fatalf("want newest-first v2,v1; got %s,%s", list[0].GetVersion(), list[1].GetVersion())
	}
	if list[0].GetCreatedAt() == nil || list[1].GetCreatedAt() == nil {
		t.Fatal("created_at must be set on register")
	}
	if !list[0].GetCreatedAt().AsTime().After(list[1].GetCreatedAt().AsTime()) {
		t.Fatal("v2 created_at should be after v1")
	}

	// Re-register same version preserves original created_at.
	prev := list[1].GetCreatedAt().AsTime()
	if err := s.RegisterArtifact(&pb.ArtifactRef{
		Name: "s", Version: "v1", Digest: "sha256:aaa2", Uri: "file:///tmp/a2",
	}); err != nil {
		t.Fatal(err)
	}
	got, ok := s.GetArtifact("s", "v1")
	if !ok || !got.GetCreatedAt().AsTime().Equal(prev) {
		t.Fatalf("re-register should preserve created_at; got %v want %v", got.GetCreatedAt().AsTime(), prev)
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
		Resources: &pb.MachineResources{CpuPercent: 12.5, MemoryUsedBytes: 1024},
		Processes: []*pb.ProcessMetrics{{Strategy: "s1", CpuPercent: 3, RssBytes: 256}},
	}, 100); err != nil {
		t.Fatal(err)
	}
	rec, _ := s.GetMachine("m1")
	if rec.ObservedGen != 7 || rec.AgentVersion != 3 || rec.LastHeartbeat != 100 ||
		rec.AgentBuildVersion != "v1.0.1-dirty" {
		t.Fatalf("unexpected record: %+v", rec)
	}
	if rec.LastResources.GetCpuPercent() != 12.5 || len(rec.LastProcesses) != 1 {
		t.Fatalf("resources not stored: %+v procs=%v", rec.LastResources, rec.LastProcesses)
	}
	samples, err := s.ListResourceSamples("m1", "", time.Unix(0, 0))
	if err != nil || len(samples) != 1 {
		t.Fatalf("machine samples: err=%v n=%d", err, len(samples))
	}
	psamples, err := s.ListResourceSamples("m1", "s1", time.Unix(0, 0))
	if err != nil || len(psamples) != 1 || psamples[0].MemBytes != 256 {
		t.Fatalf("process samples: err=%v %+v", err, psamples)
	}
	// Within the sample interval: no second write.
	if err := s.ApplyHeartbeat("m1", &pb.Heartbeat{
		Resources: &pb.MachineResources{CpuPercent: 99},
	}, 130); err != nil {
		t.Fatal(err)
	}
	samples, _ = s.ListResourceSamples("m1", "", time.Unix(0, 0))
	if len(samples) != 1 {
		t.Fatalf("expected rate-limited samples, got %d", len(samples))
	}
	// Past interval: append.
	if err := s.ApplyHeartbeat("m1", &pb.Heartbeat{
		Resources: &pb.MachineResources{CpuPercent: 40},
	}, 100+int64(ResourceSampleInterval/time.Second)); err != nil {
		t.Fatal(err)
	}
	samples, _ = s.ListResourceSamples("m1", "", time.Unix(0, 0))
	if len(samples) != 2 {
		t.Fatalf("expected 2 samples, got %d", len(samples))
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

func TestApplyStatusPrunesRetiredStrategies(t *testing.T) {
	s := NewMemory(nil)
	s.UpsertMachine(&pb.Register{MachineId: "m1"})
	if err := s.ApplyStatus("m1", &pb.StatusReport{
		ObservedGeneration: 1,
		Assignments: []*pb.StrategyAssignmentStatus{
			{Strategy: "fleet1", Phase: pb.DeployPhase_DEPLOY_PHASE_DRAINING, Pid: 1},
			{Strategy: "fleet2", Phase: pb.DeployPhase_DEPLOY_PHASE_HEALTHY, Pid: 2},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Agent finished draining fleet1 and omits it from the next snapshot.
	if err := s.ApplyStatus("m1", &pb.StatusReport{
		ObservedGeneration: 2,
		Assignments: []*pb.StrategyAssignmentStatus{
			{Strategy: "fleet2", Phase: pb.DeployPhase_DEPLOY_PHASE_HEALTHY, Pid: 2},
		},
	}); err != nil {
		t.Fatal(err)
	}
	rec, _ := s.GetMachine("m1")
	if _, ok := rec.Status["fleet1"]; ok {
		t.Fatalf("fleet1 status should be pruned after retire, got %+v", rec.Status["fleet1"])
	}
	if rec.Status["fleet2"].GetPid() != 2 {
		t.Fatalf("fleet2 status = %+v", rec.Status["fleet2"])
	}
}
