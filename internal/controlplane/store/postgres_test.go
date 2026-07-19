package store

import (
	"context"
	"os"
	"testing"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// newTestPostgres connects to the DSN in STRATEGON_TEST_DB, runs migrations,
// and truncates all data so each test starts clean. Skips when unset.
func newTestPostgres(t *testing.T, hub *Hub) *Postgres {
	t.Helper()
	dsn := os.Getenv("STRATEGON_TEST_DB")
	if dsn == "" {
		t.Skip("STRATEGON_TEST_DB not set; skipping Postgres store tests")
	}
	ctx := context.Background()
	p, err := NewPostgres(ctx, dsn, hub)
	if err != nil {
		t.Fatalf("NewPostgres: %v", err)
	}
	if _, err := p.pool.Exec(ctx, `TRUNCATE machines, artifacts, audit, leases RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func TestPostgresGenerationBumpAndDesired(t *testing.T) {
	p := newTestPostgres(t, nil)
	if _, err := p.UpsertMachine(&pb.Register{MachineId: "m1", AgentVersion: 2}); err != nil {
		t.Fatal(err)
	}
	spec := &pb.StrategyAssignmentSpec{Strategy: "s", Artifact: &pb.ArtifactRef{Version: "v1", Digest: "sha256:aaa"}}
	g1, err := p.SetAssignment("m1", "s", spec)
	if err != nil {
		t.Fatal(err)
	}
	if g1 != 1 {
		t.Fatalf("first generation = %d, want 1", g1)
	}
	g2, _ := p.SetAssignment("m1", "s2", &pb.StrategyAssignmentSpec{Strategy: "s2", Artifact: &pb.ArtifactRef{Version: "v1", Digest: "sha256:bbb"}})
	if g2 != 2 {
		t.Fatalf("second generation = %d, want 2", g2)
	}
	ds, ok := p.DesiredState("m1")
	if !ok || ds.GetGeneration() != 2 || len(ds.GetAssignments()) != 2 {
		t.Fatalf("desired = %+v ok=%v", ds, ok)
	}
	// assignments sorted by strategy
	if ds.GetAssignments()[0].GetStrategy() != "s" || ds.GetAssignments()[1].GetStrategy() != "s2" {
		t.Fatalf("assignments not sorted: %v", ds.GetAssignments())
	}
	if _, ok := p.GetMachine("m1"); !ok {
		t.Fatal("machine should exist")
	}
	if _, ok := p.GetMachine("nope"); ok {
		t.Fatal("unknown machine should be absent")
	}
}

func TestPostgresPreviousArtifactAndRollback(t *testing.T) {
	p := newTestPostgres(t, nil)
	p.UpsertMachine(&pb.Register{MachineId: "m1"})
	p.SetAssignment("m1", "s", &pb.StrategyAssignmentSpec{Strategy: "s", Artifact: &pb.ArtifactRef{Version: "v1", Digest: "sha256:aaa"}})
	if _, ok := p.PreviousArtifact("m1", "s"); ok {
		t.Fatal("no previous artifact after first deploy")
	}
	p.SetAssignment("m1", "s", &pb.StrategyAssignmentSpec{Strategy: "s", Artifact: &pb.ArtifactRef{Version: "v2", Digest: "sha256:bbb"}})
	prev, ok := p.PreviousArtifact("m1", "s")
	if !ok || prev.GetVersion() != "v1" {
		t.Fatalf("previous artifact = %+v ok=%v, want v1", prev, ok)
	}
	// Redeploy same digest must not overwrite previous.
	p.SetAssignment("m1", "s", &pb.StrategyAssignmentSpec{Strategy: "s", Artifact: &pb.ArtifactRef{Version: "v2", Digest: "sha256:bbb"}})
	prev, _ = p.PreviousArtifact("m1", "s")
	if prev.GetVersion() != "v1" {
		t.Fatalf("previous should stay v1, got %s", prev.GetVersion())
	}
	// Removing the assignment clears previous.
	p.SetAssignment("m1", "s", nil)
	if _, ok := p.PreviousArtifact("m1", "s"); ok {
		t.Fatal("previous artifact should be cleared on removal")
	}
}

func TestPostgresStatusHeartbeatReachable(t *testing.T) {
	p := newTestPostgres(t, nil)
	p.UpsertMachine(&pb.Register{MachineId: "m1"})
	if err := p.ApplyStatus("m1", &pb.StatusReport{
		ObservedGeneration: 3,
		Assignments:        []*pb.StrategyAssignmentStatus{{Strategy: "s", Phase: pb.DeployPhase_DEPLOY_PHASE_HEALTHY}},
	}); err != nil {
		t.Fatal(err)
	}
	rec, _ := p.GetMachine("m1")
	if rec.ObservedGen != 3 || rec.Status["s"].GetPhase() != pb.DeployPhase_DEPLOY_PHASE_HEALTHY {
		t.Fatalf("status not recorded: %+v", rec)
	}
	// observed_gen is monotonic (a lower report must not lower it).
	p.ApplyStatus("m1", &pb.StatusReport{ObservedGeneration: 1})
	rec, _ = p.GetMachine("m1")
	if rec.ObservedGen != 3 {
		t.Fatalf("observed_gen regressed to %d", rec.ObservedGen)
	}
	if err := p.ApplyHeartbeat("m1", &pb.Heartbeat{AgentVersion: 7, ObservedGeneration: 5}, 1000); err != nil {
		t.Fatal(err)
	}
	rec, _ = p.GetMachine("m1")
	if rec.LastHeartbeat != 1000 || rec.AgentVersion != 7 || rec.ObservedGen != 5 || !rec.Reachable {
		t.Fatalf("heartbeat not recorded: %+v", rec)
	}
	p.SetReachable("m1", false)
	rec, _ = p.GetMachine("m1")
	if rec.Reachable {
		t.Fatal("should be unreachable")
	}
	// unknown-machine errors
	if err := p.ApplyStatus("nope", &pb.StatusReport{}); err == nil {
		t.Fatal("ApplyStatus on unknown machine should error")
	}
	if err := p.ApplyHeartbeat("nope", &pb.Heartbeat{}, 1); err == nil {
		t.Fatal("ApplyHeartbeat on unknown machine should error")
	}
	if err := p.SetReachable("nope", true); err == nil {
		t.Fatal("SetReachable on unknown machine should error")
	}
	if _, err := p.SetAssignment("nope", "s", &pb.StrategyAssignmentSpec{}); err == nil {
		t.Fatal("SetAssignment on unknown machine should error")
	}
}

func TestPostgresAuditOrderingAndFilter(t *testing.T) {
	p := newTestPostgres(t, nil)
	p.AppendAudit(&pb.AuditEntry{MachineId: "m1", Strategy: "s", Action: "Deploy", Timestamp: timestamppb.New(time.Unix(10, 0))})
	p.AppendAudit(&pb.AuditEntry{MachineId: "m1", Strategy: "s2", Action: "Deploy", Timestamp: timestamppb.New(time.Unix(20, 0))})
	p.AppendAudit(&pb.AuditEntry{MachineId: "m2", Strategy: "s", Action: "Rollback", Timestamp: timestamppb.New(time.Unix(30, 0))})

	all := p.ListAudit("", "")
	if len(all) != 3 || all[0].GetAction() != "Rollback" {
		t.Fatalf("expected 3 newest-first, got %d first=%v", len(all), all[0].GetAction())
	}
	byMachine := p.ListAudit("m1", "")
	if len(byMachine) != 2 {
		t.Fatalf("filter by machine = %d, want 2", len(byMachine))
	}
	byStrat := p.ListAudit("m1", "s2")
	if len(byStrat) != 1 || byStrat[0].GetStrategy() != "s2" {
		t.Fatalf("filter by strategy = %v", byStrat)
	}
}

func TestPostgresArtifactCatalog(t *testing.T) {
	p := newTestPostgres(t, nil)
	if err := p.RegisterArtifact(&pb.ArtifactRef{Name: "s", Version: "v1", Digest: "sha256:aaa", Uri: "file:///tmp/x"}); err != nil {
		t.Fatal(err)
	}
	if err := p.RegisterArtifact(&pb.ArtifactRef{Name: "s", Version: "v2", Digest: "sha256:bbb", Uri: "https://github.com/o/r/releases/download/v2/s"}); err != nil {
		t.Fatal(err)
	}
	if err := p.RegisterArtifact(&pb.ArtifactRef{Name: "s", Version: "v3", Digest: "sha256:ccc", Uri: "file://tmp/x"}); err == nil {
		t.Fatal("relative file:// uri should be rejected")
	}
	got, ok := p.GetArtifact("s", "v2")
	if !ok || got.GetUri() != "https://github.com/o/r/releases/download/v2/s" {
		t.Fatalf("GetArtifact = %+v ok=%v", got, ok)
	}
	list := p.ListArtifacts("s")
	if len(list) != 2 || list[0].GetVersion() != "v1" || list[1].GetVersion() != "v2" {
		t.Fatalf("ListArtifacts = %v, want v1,v2 sorted", list)
	}
}

func TestPostgresDurabilityAcrossReconnect(t *testing.T) {
	dsn := os.Getenv("STRATEGON_TEST_DB")
	p := newTestPostgres(t, nil)
	p.UpsertMachine(&pb.Register{MachineId: "m1"})
	p.SetAssignment("m1", "s", &pb.StrategyAssignmentSpec{Strategy: "s", Artifact: &pb.ArtifactRef{Version: "v1", Digest: "sha256:aaa"}})
	p.Close()

	// Reconnect: state must survive (the whole point of Postgres over Memory).
	p2, err := NewPostgres(context.Background(), dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p2.Close)
	ds, ok := p2.DesiredState("m1")
	if !ok || ds.GetGeneration() != 1 || len(ds.GetAssignments()) != 1 {
		t.Fatalf("state did not survive reconnect: %+v ok=%v", ds, ok)
	}
}

func TestPostgresLeaseSurvivesReconnect(t *testing.T) {
	dsn := os.Getenv("STRATEGON_TEST_DB")
	p := newTestPostgres(t, nil)
	res, err := p.AcquireLease("m1", "s", time.Minute)
	if err != nil || !res.Granted {
		t.Fatalf("acquire: %+v err=%v", res, err)
	}
	leaseID := res.LeaseID
	p.Close()

	p2, err := NewPostgres(context.Background(), dsn, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p2.Close)

	info, ok := p2.GetLease("s")
	if !ok || info.MachineID != "m1" || info.LeaseID != leaseID {
		t.Fatalf("lease lost across reconnect: %+v ok=%v", info, ok)
	}
	denied, err := p2.AcquireLease("m2", "s", time.Minute)
	if err != nil || denied.Granted {
		t.Fatalf("m2 should still be denied after CP restart: %+v err=%v", denied, err)
	}
	renewed, err := p2.RenewLease("m1", "s", leaseID, 0)
	if err != nil || !renewed.Granted {
		t.Fatalf("renew after reconnect: %+v err=%v", renewed, err)
	}
}
