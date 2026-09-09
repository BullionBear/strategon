package orchestrator

import (
	"context"
	"strings"
	"testing"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/controlplane/assign"
	"github.com/bullionbear/strategon/internal/controlplane/store"
)

func setupCluster(t *testing.T, n int) (*Controller, *store.Memory, *assign.Service) {
	t.Helper()
	st := store.NewMemory(nil)
	if err := st.RegisterArtifact(&pb.ArtifactRef{
		Name: "nats", Version: "v1", Digest: "sha256:nats1", Uri: "file:///nats-v1",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterArtifact(&pb.ArtifactRef{
		Name: "nats-config", Version: "c1", Digest: "sha256:cfg1", Uri: "file:///nats.conf",
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterArtifact(&pb.ArtifactRef{
		Name: "nats", Version: "v2", Digest: "sha256:nats2", Uri: "file:///nats-v2",
	}); err != nil {
		t.Fatal(err)
	}
	servers := make([]*pb.NatsServer, 0, n)
	for i := 1; i <= n; i++ {
		id := machineID(i)
		if _, err := st.UpsertMachine(&pb.Register{MachineId: id}); err != nil {
			t.Fatal(err)
		}
		servers = append(servers, &pb.NatsServer{
			Machine: id, ServerName: "nats-" + id, RouteHost: "10.0.0." + strings.TrimPrefix(id, "m"),
			ClientPort: 4222, ClusterPort: 6222, MonitorPort: 8222,
		})
	}
	if _, _, err := st.ApplyNatsCluster(&pb.NatsCluster{
		Metadata: &pb.ObjectMeta{Name: "trading"},
		Spec: &pb.NatsClusterSpec{
			ArtifactVersion: "v1",
			ConfigVersion:   "c1",
			Strategy:        "nats",
			Servers:         servers,
			Update:          &pb.NatsClusterUpdate{MaxUnavailable: 1, WaitReadySeconds: 30},
		},
	}); err != nil {
		t.Fatal(err)
	}
	asg := assign.New(st, nil)
	ctrl := New(st, asg, nil, nil)
	return ctrl, st, asg
}

func machineID(i int) string { return "m" + string(rune('0'+i)) } // m1..m9

func loadCluster(t *testing.T, st store.Store) *pb.NatsCluster {
	t.Helper()
	c, ok := st.GetNatsCluster("trading")
	if !ok {
		t.Fatal("cluster missing")
	}
	return c
}

func assigned(st store.Store, machine string) bool {
	rec, ok := st.GetMachine(machine)
	return ok && rec.Assignments["nats"] != nil
}

func markReady(t *testing.T, st store.Store, machine string, ready bool, phase pb.DeployPhase) {
	t.Helper()
	rec, ok := st.GetMachine(machine)
	if !ok {
		t.Fatal("machine", machine)
	}
	spec := rec.Assignments["nats"]
	if spec == nil {
		t.Fatal("no assignment on", machine)
	}
	readyStatus := pb.ConditionStatus_CONDITION_STATUS_FALSE
	if ready {
		readyStatus = pb.ConditionStatus_CONDITION_STATUS_TRUE
	}
	if err := st.ApplyStatus(machine, &pb.StatusReport{
		ObservedGeneration: rec.Generation,
		Assignments: []*pb.StrategyAssignmentStatus{{
			Strategy:           "nats",
			Phase:              phase,
			RunningArtifact:    spec.GetArtifact(),
			RunningConfig:      spec.GetConfig(),
			ObservedGeneration: rec.Generation,
			Conditions: []*pb.Condition{
				{Type: "Live", Status: pb.ConditionStatus_CONDITION_STATUS_TRUE},
				{Type: "Ready", Status: readyStatus},
			},
		}},
	}); err != nil {
		t.Fatal(err)
	}
}

func TestReconcileWritesOneThenNextAfterReady(t *testing.T) {
	ctrl, st, _ := setupCluster(t, 3)
	ctx := context.Background()
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	if !assigned(st, "m1") || assigned(st, "m2") || assigned(st, "m3") {
		t.Fatalf("first pass assignments: m1=%v m2=%v m3=%v", assigned(st, "m1"), assigned(st, "m2"), assigned(st, "m3"))
	}

	// Converged but Ready FALSE must not advance.
	markReady(t, st, "m1", false, pb.DeployPhase_DEPLOY_PHASE_HEALTHY)
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	if assigned(st, "m2") {
		t.Fatal("must not write next while Ready is FALSE")
	}

	markReady(t, st, "m1", true, pb.DeployPhase_DEPLOY_PHASE_HEALTHY)
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	if !assigned(st, "m2") || assigned(st, "m3") {
		t.Fatalf("second pass: m2=%v m3=%v", assigned(st, "m2"), assigned(st, "m3"))
	}
}

func TestWaitReadyDeadlineDegrades(t *testing.T) {
	ctrl, st, _ := setupCluster(t, 3)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ctrl.Now = func() time.Time { return base }
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	ctrl.Now = func() time.Time { return base.Add(31 * time.Second) }
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	cl := loadCluster(t, st)
	if cl.GetStatus().GetPhase() != "Degraded" {
		t.Fatalf("phase=%s, want Degraded", cl.GetStatus().GetPhase())
	}
	if assigned(st, "m2") {
		t.Fatal("deadline must not write further members")
	}
}

func TestRolledBackStopsRoll(t *testing.T) {
	ctrl, st, _ := setupCluster(t, 3)
	ctx := context.Background()
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	markReady(t, st, "m1", false, pb.DeployPhase_DEPLOY_PHASE_ROLLED_BACK)
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	cl := loadCluster(t, st)
	if cl.GetStatus().GetPhase() != "Failed" {
		t.Fatalf("phase=%s", cl.GetStatus().GetPhase())
	}
	if assigned(st, "m2") {
		t.Fatal("ROLLED_BACK must stop the roll")
	}
}

func TestFailedStopsRoll(t *testing.T) {
	ctrl, st, _ := setupCluster(t, 3)
	ctx := context.Background()
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	markReady(t, st, "m1", false, pb.DeployPhase_DEPLOY_PHASE_FAILED)
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	if assigned(st, "m2") {
		t.Fatal("FAILED must stop the roll")
	}
}

func TestGeneratedArgsAndEnv(t *testing.T) {
	ctrl, st, _ := setupCluster(t, 3)
	if err := ctrl.Reconcile(context.Background(), loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	rec, _ := st.GetMachine("m1")
	spec := rec.Assignments["nats"]
	if spec == nil {
		t.Fatal("missing assignment")
	}
	joined := strings.Join(spec.GetArgs(), " ")
	if !strings.Contains(joined, "-c ${CONFIG}") {
		t.Fatalf("args = %v, want -c ${CONFIG}", spec.GetArgs())
	}
	if !strings.Contains(joined, "--routes") {
		t.Fatalf("args = %v, want --routes", spec.GetArgs())
	}
	if strings.Contains(joined, "nats://10.0.0.1:") {
		t.Fatalf("routes include self: %v", spec.GetArgs())
	}
	if _, ok := spec.GetEnv()["NATS_ROUTES"]; ok {
		t.Fatal("must not emit NATS_ROUTES")
	}
	if spec.GetEnv()["NATS_SERVER_NAME"] != "nats-m1" || spec.GetReadiness().GetEndpoint() != "http://127.0.0.1:8222/healthz" {
		t.Fatalf("env/probe = %+v readiness=%+v", spec.GetEnv(), spec.GetReadiness())
	}
	if spec.GetStopped() || spec.GetLease() != nil {
		t.Fatal("stopped/lease unexpected")
	}
}

func TestVersionBumpRollsOneAtATime(t *testing.T) {
	ctrl, st, _ := setupCluster(t, 3)
	ctx := context.Background()
	for _, m := range []string{"m1", "m2", "m3"} {
		if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
			t.Fatal(err)
		}
		markReady(t, st, m, true, pb.DeployPhase_DEPLOY_PHASE_HEALTHY)
	}
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	if loadCluster(t, st).GetStatus().GetPhase() != "Ready" {
		t.Fatalf("phase=%s", loadCluster(t, st).GetStatus().GetPhase())
	}

	if _, _, err := st.ApplyNatsCluster(&pb.NatsCluster{
		Metadata: &pb.ObjectMeta{Name: "trading"},
		Spec: &pb.NatsClusterSpec{
			ArtifactVersion: "v2",
			ConfigVersion:   "c1",
			Strategy:        "nats",
			Servers:         loadCluster(t, st).GetSpec().GetServers(),
			Update:          &pb.NatsClusterUpdate{MaxUnavailable: 1, WaitReadySeconds: 30},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	v2 := 0
	for _, m := range []string{"m1", "m2", "m3"} {
		rec, _ := st.GetMachine(m)
		if rec.Assignments["nats"].GetArtifact().GetVersion() == "v2" {
			v2++
		}
	}
	if v2 != 1 {
		t.Fatalf("upgrade wrote %d v2 assignments, want 1", v2)
	}
}

func TestRestartOnlyWritesRemaining(t *testing.T) {
	_, st, asg := setupCluster(t, 3)
	ctx := context.Background()
	cl := loadCluster(t, st)
	art, cfg, err := resolveClusterArtifacts(st, cl)
	if err != nil {
		t.Fatal(err)
	}
	servers := cl.GetSpec().GetServers()
	for i, srv := range servers {
		if i == 2 {
			break
		}
		spec := computeAssignment(cl, srv, servers, art, cfg)
		if _, _, err := asg.Apply(ctx, assign.Request{
			MachineID: srv.GetMachine(), Strategy: "nats", Spec: spec, AllowReserved: true,
		}); err != nil {
			t.Fatal(err)
		}
		markReady(t, st, srv.GetMachine(), true, pb.DeployPhase_DEPLOY_PHASE_HEALTHY)
	}
	fresh := New(st, asg, nil, nil)
	if err := fresh.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	if !assigned(st, "m3") {
		t.Fatal("remaining member not written")
	}
	// m1/m2 already matched — no extra generation from rewrite (E1 no-op).
}

func TestPrunedStatusTreatedAsAbsent(t *testing.T) {
	ctrl, st, _ := setupCluster(t, 1)
	ctx := context.Background()
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.SetAssignment("m1", "nats", nil); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyStatus("m1", &pb.StatusReport{ObservedGeneration: 1}); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	if !assigned(st, "m1") {
		t.Fatal("pruned member should be rewritten as absent")
	}
}

func TestReadyClusterDoesNotRewrite(t *testing.T) {
	ctrl, st, _ := setupCluster(t, 1)
	ctx := context.Background()
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	markReady(t, st, "m1", true, pb.DeployPhase_DEPLOY_PHASE_HEALTHY)
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	rec, _ := st.GetMachine("m1")
	gen := rec.Generation
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	rec, _ = st.GetMachine("m1")
	if rec.Generation != gen {
		t.Fatalf("Ready cluster rewrote assignment: gen %d → %d", gen, rec.Generation)
	}
}

func TestDesiredStateRollOrder(t *testing.T) {
	ctrl, st, _ := setupCluster(t, 3)
	ctx := context.Background()
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	ds, ok := st.DesiredState("m1")
	if !ok || len(ds.GetAssignments()) != 1 {
		t.Fatalf("m1 desired = %+v", ds)
	}
	ds2, _ := st.DesiredState("m2")
	if len(ds2.GetAssignments()) != 0 {
		t.Fatal("m2 should still be empty")
	}
}

func TestComputeAssignmentCreatedAtStable(t *testing.T) {
	// CreatedAt on catalog refs should not prevent proto.Equal after write.
	ctrl, st, _ := setupCluster(t, 2)
	ctx := context.Background()
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	markReady(t, st, "m1", true, pb.DeployPhase_DEPLOY_PHASE_HEALTHY)
	rec, _ := st.GetMachine("m1")
	gen := rec.Generation
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	rec, _ = st.GetMachine("m1")
	if rec.Generation != gen {
		t.Fatalf("matching node rewritten: %d → %d", gen, rec.Generation)
	}
}
