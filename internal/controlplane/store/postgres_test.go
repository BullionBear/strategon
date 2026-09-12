package store

import (
	"context"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/jackc/pgx/v5/pgconn"
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
	if _, err := p.pool.Exec(ctx, `TRUNCATE machines, artifacts, audit, leases, api_tokens, resource_samples, assignment_sets RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	t.Cleanup(p.Close)
	return p
}

func TestPostgresGenerationBumpAndDesired(t *testing.T) {
	p := newTestPostgres(t, nil)
	if _, err := p.UpsertMachine(&pb.Register{MachineId: "m1", AgentVersion: 2, AgentBuildVersion: "dev"}); err != nil {
		t.Fatal(err)
	}
	spec := &pb.StrategyAssignmentSpec{Strategy: "s", Artifact: &pb.ArtifactRef{Version: "v1", Digest: "sha256:aaa"}}
	g1, _, err := p.SetAssignment("m1", "s", spec)
	if err != nil {
		t.Fatal(err)
	}
	if g1 != 1 {
		t.Fatalf("first generation = %d, want 1", g1)
	}
	g2, _, _ := p.SetAssignment("m1", "s2", &pb.StrategyAssignmentSpec{Strategy: "s2", Artifact: &pb.ArtifactRef{Version: "v1", Digest: "sha256:bbb"}})
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

func TestPostgresCreateDeleteVolumeDesiredNilVsEmpty(t *testing.T) {
	p := newTestPostgres(t, nil)
	if _, err := p.UpsertMachine(&pb.Register{MachineId: "m1", AgentVersion: 1}); err != nil {
		t.Fatal(err)
	}
	ds, _ := p.DesiredState("m1")
	if ds.GetVolumes() != nil {
		t.Fatalf("fresh machine must send nil volumes, got %+v", ds.GetVolumes())
	}
	vg1, _, changed, err := p.CreateVolume("m1", "data")
	if err != nil || !changed || vg1 != 1 {
		t.Fatalf("create volGen=%d changed=%v err=%v", vg1, changed, err)
	}
	ds, _ = p.DesiredState("m1")
	if ds.GetVolumes() == nil || len(ds.GetVolumes().GetVolumes()) != 1 {
		t.Fatalf("desired volumes = %+v", ds.GetVolumes())
	}
	if _, _, changed, err = p.CreateVolume("m1", "data"); err != nil || changed {
		t.Fatalf("identical create should be no-op: changed=%v err=%v", changed, err)
	}
	vg2, _, changed, err := p.DeleteVolume("m1", "data")
	if err != nil || !changed || vg2 != 2 {
		t.Fatalf("delete volGen=%d changed=%v err=%v", vg2, changed, err)
	}
	ds, _ = p.DesiredState("m1")
	if ds.GetVolumes() == nil || ds.GetVolumes().GetGeneration() != 2 || len(ds.GetVolumes().GetVolumes()) != 0 {
		t.Fatalf("empty list must still be sent: %+v", ds.GetVolumes())
	}
}

func TestPostgresStatusHeartbeatReachable(t *testing.T) {
	p := newTestPostgres(t, nil)
	p.UpsertMachine(&pb.Register{MachineId: "m1"})
	if err := p.ApplyStatus("m1", &pb.StatusReport{
		ObservedGeneration: 3,
		Assignments:        []*pb.StrategyAssignmentStatus{{Strategy: "s", Phase: pb.DeployPhase_DEPLOY_PHASE_HEALTHY}},
		Slots: &pb.MachineSlotStatus{
			Slots: []*pb.StrategySlotStatus{{Strategy: "probe-fail", SizeBytes: 12}},
		},
	}); err != nil {
		t.Fatal(err)
	}
	rec, _ := p.GetMachine("m1")
	if rec.ObservedGen != 3 || rec.Status["s"].GetPhase() != pb.DeployPhase_DEPLOY_PHASE_HEALTHY {
		t.Fatalf("status not recorded: %+v", rec)
	}
	if rec.SlotsStatus == nil || rec.SlotsStatus.GetSlots()[0].GetStrategy() != "probe-fail" {
		t.Fatalf("slots status not recorded: %+v", rec.SlotsStatus)
	}
	if err := p.ApplyStatus("m1", &pb.StatusReport{ObservedGeneration: 3}); err != nil {
		t.Fatal(err)
	}
	rec, _ = p.GetMachine("m1")
	if rec.SlotsStatus == nil || rec.SlotsStatus.GetSlots()[0].GetStrategy() != "probe-fail" {
		t.Fatalf("nil slots must not wipe: %+v", rec.SlotsStatus)
	}
	// observed_gen is monotonic (a lower report must not lower it).
	p.ApplyStatus("m1", &pb.StatusReport{ObservedGeneration: 1})
	rec, _ = p.GetMachine("m1")
	if rec.ObservedGen != 3 {
		t.Fatalf("observed_gen regressed to %d", rec.ObservedGen)
	}
	if err := p.ApplyHeartbeat("m1", &pb.Heartbeat{
		AgentVersion: 7, AgentBuildVersion: "v9", ObservedGeneration: 5,
	}, 1000); err != nil {
		t.Fatal(err)
	}
	rec, _ = p.GetMachine("m1")
	if rec.LastHeartbeat != 1000 || rec.AgentVersion != 7 || rec.AgentBuildVersion != "v9" ||
		rec.ObservedGen != 5 || !rec.Reachable {
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
	if _, _, err := p.SetAssignment("nope", "s", &pb.StrategyAssignmentSpec{}); err == nil {
		t.Fatal("SetAssignment on unknown machine should error")
	}
}

func TestPostgresUndeployAndStatusNoDeadlock(t *testing.T) {
	p := newTestPostgres(t, nil)
	if _, err := p.UpsertMachine(&pb.Register{MachineId: "m1"}); err != nil {
		t.Fatal(err)
	}
	strategies := []string{"s1", "s2", "s3", "s4", "s5"}
	statuses := make([]*pb.StrategyAssignmentStatus, 0, len(strategies))
	for _, s := range strategies {
		if _, _, err := p.SetAssignment("m1", s, &pb.StrategyAssignmentSpec{
			Strategy: s,
			Artifact: &pb.ArtifactRef{Version: "v1", Digest: "sha256:aaa"},
		}); err != nil {
			t.Fatal(err)
		}
		statuses = append(statuses, &pb.StrategyAssignmentStatus{
			Strategy: s,
			Phase:    pb.DeployPhase_DEPLOY_PHASE_HEALTHY,
		})
	}
	if err := p.ApplyStatus("m1", &pb.StatusReport{ObservedGeneration: 1, Assignments: statuses}); err != nil {
		t.Fatal(err)
	}

	errs := make(chan error, len(strategies)+8)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for n := 0; n < 20; n++ {
				if err := p.ApplyStatus("m1", &pb.StatusReport{
					ObservedGeneration: int64(n + 1),
					Assignments:        statuses,
				}); err != nil {
					errs <- err
					return
				}
			}
		}()
	}
	for _, s := range strategies {
		s := s
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, err := p.SetAssignment("m1", s, nil); err != nil {
				errs <- err
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "40P01" {
			t.Fatalf("deadlock: %v", err)
		}
		t.Fatalf("store write: %v", err)
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
	if len(list) != 2 || list[0].GetVersion() != "v2" || list[1].GetVersion() != "v1" {
		t.Fatalf("ListArtifacts = %v, want v2,v1 newest-first", versionsOf(list))
	}
	if list[0].GetCreatedAt() == nil || list[1].GetCreatedAt() == nil {
		t.Fatal("created_at must be set on register")
	}
}

func versionsOf(list []*pb.ArtifactRef) []string {
	out := make([]string, len(list))
	for i, a := range list {
		out[i] = a.GetVersion()
	}
	return out
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

func TestPostgresAPITokens(t *testing.T) {
	p := newTestPostgres(t, nil)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	row := TokenRow{
		ID:        "abcd1234abcd1234",
		TokenHash: "deadbeef",
		Name:      "ci",
		UserID:    "u1",
		Username:  "alice",
		CreatedAt: now,
	}
	if err := p.InsertAPIToken(ctx, row); err != nil {
		t.Fatal(err)
	}
	loaded, err := p.LoadAPITokens(ctx)
	if err != nil || len(loaded) != 1 || loaded[0].ID != row.ID || loaded[0].TokenHash != row.TokenHash {
		t.Fatalf("load: %+v err=%v", loaded, err)
	}
	if err := p.TouchAPITokens(ctx, map[string]time.Time{row.ID: now.Add(time.Minute)}); err != nil {
		t.Fatal(err)
	}
	loaded, err = p.LoadAPITokens(ctx)
	if err != nil || len(loaded) != 1 || loaded[0].LastUsed.IsZero() {
		t.Fatalf("touch: %+v err=%v", loaded, err)
	}
	ok, err := p.RevokeAPIToken(ctx, "other", row.ID)
	if err != nil || ok {
		t.Fatalf("cross-user revoke ok=%v err=%v", ok, err)
	}
	ok, err = p.RevokeAPIToken(ctx, "u1", row.ID)
	if err != nil || !ok {
		t.Fatalf("revoke ok=%v err=%v", ok, err)
	}
	loaded, err = p.LoadAPITokens(ctx)
	if err != nil || len(loaded) != 0 {
		t.Fatalf("active after revoke: %+v err=%v", loaded, err)
	}
}

func TestPostgresAssignmentSetConcurrentApplyRejectsOverlap(t *testing.T) {
	assertConcurrentOverlapRejected(t, newTestPostgres(t, nil))
}

func TestPostgresAssignmentSetReapplyAndGrowth(t *testing.T) {
	assertReapplyAndGrowth(t, newTestPostgres(t, nil))
}

func TestPostgresAssignmentSetStatusPreservesDeleting(t *testing.T) {
	p := newTestPostgres(t, nil)
	if _, err := applySet(p, "trading", setSpec("m1")); err != nil {
		t.Fatal(err)
	}
	if _, err := p.MarkAssignmentSetDeleting("trading"); err != nil {
		t.Fatal(err)
	}
	if err := p.UpdateAssignmentSetStatus("trading", &pb.AssignmentSetStatus{
		Phase:              "Ready",
		ObservedGeneration: 1,
		Members:            []*pb.MemberStatus{{Machine: "m1", Ready: true}},
	}); err != nil {
		t.Fatal(err)
	}
	got, ok := p.GetAssignmentSet("trading")
	if !ok {
		t.Fatal("missing cluster")
	}
	if !got.GetStatus().GetDeleting() || got.GetStatus().GetPhase() != "Deleting" {
		t.Fatalf("status = %+v, want Deleting preserved", got.GetStatus())
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
