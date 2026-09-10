package orchestrator

import (
	"context"
	"sort"
	"strconv"
	"strings"
	"testing"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/controlplane/assign"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"google.golang.org/protobuf/proto"
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
	servers := make([]*pb.SetMember, 0, n)
	for i := 1; i <= n; i++ {
		id := machineID(i)
		if _, err := st.UpsertMachine(&pb.Register{MachineId: id}); err != nil {
			t.Fatal(err)
		}
		servers = append(servers, &pb.SetMember{
			Machine: id, Name: "nats-" + id,
			Vars: map[string]string{"route_host": "10.0.0." + strings.TrimPrefix(id, "m"), "cluster_port": "6222", "monitor_port": "8222"},
		})
	}
	if _, _, err := st.ApplyAssignmentSet(&pb.AssignmentSet{
		Metadata: &pb.ObjectMeta{Name: "trading"},
		Spec: &pb.AssignmentSetSpec{
			ArtifactVersion: "v1",
			ConfigVersion:   "c1",
			Strategy:        "nats",
			Template:        natsTemplate(),
			Members:         servers,
			Update:          &pb.RollingUpdate{MaxUnavailable: 1, WaitReadySeconds: 30},
		},
	}); err != nil {
		t.Fatal(err)
	}
	asg := assign.New(st, nil)
	ctrl := New(st, asg, nil, nil)
	return ctrl, st, asg
}

// natsTemplate mirrors examples/nats/cluster.yaml: every NATS-specific name
// lives in the manifest, not in the controller.
func natsTemplate() *pb.MemberTemplate {
	return &pb.MemberTemplate{
		Args: []string{"-c", "${CONFIG}", "--routes", "${peers}"},
		Env: map[string]string{
			"NATS_SERVER_NAME":  "${member.name}",
			"NATS_CLUSTER_NAME": "${set.name}",
			"NATS_CLUSTER_PORT": "${member.vars.cluster_port}",
			"NATS_MONITOR_PORT": "${member.vars.monitor_port}",
		},
		Readiness: &pb.ReadinessProbe{Endpoint: "http://127.0.0.1:${member.vars.monitor_port}/healthz"},
		Peers:     &pb.PeerList{Format: "nats://${peer.vars.route_host}:${peer.vars.cluster_port}"},
	}
}

func machineID(i int) string { return "m" + string(rune('0'+i)) } // m1..m9

func loadCluster(t *testing.T, st store.Store) *pb.AssignmentSet {
	t.Helper()
	c, ok := st.GetAssignmentSet("trading")
	if !ok {
		t.Fatal("cluster missing")
	}
	return c
}

func slotName(machine string) string { return "nats-" + machine }

func assigned(st store.Store, machine string) bool {
	return assignedSlot(st, machine, slotName(machine))
}

func assignedSlot(st store.Store, machine, strategy string) bool {
	rec, ok := st.GetMachine(machine)
	return ok && rec.Assignments[strategy] != nil
}

func markReady(t *testing.T, st store.Store, machine string, ready bool, phase pb.DeployPhase) {
	t.Helper()
	markReadySlot(t, st, machine, slotName(machine), ready, phase)
}

func markReadySlot(t *testing.T, st store.Store, machine, strategy string, ready bool, phase pb.DeployPhase) {
	t.Helper()
	rec, ok := st.GetMachine(machine)
	if !ok {
		t.Fatal("machine", machine)
	}
	spec := rec.Assignments[strategy]
	if spec == nil {
		t.Fatal("no assignment", strategy, "on", machine)
	}
	readyStatus := pb.ConditionStatus_CONDITION_STATUS_FALSE
	if ready {
		readyStatus = pb.ConditionStatus_CONDITION_STATUS_TRUE
	}
	statuses := make([]*pb.StrategyAssignmentStatus, 0, len(rec.Assignments))
	for name, as := range rec.Assignments {
		stName := name
		stSpec := as
		phaseOut := pb.DeployPhase_DEPLOY_PHASE_HEALTHY
		readyOut := pb.ConditionStatus_CONDITION_STATUS_TRUE
		if name == strategy {
			phaseOut = phase
			readyOut = readyStatus
			stSpec = spec
		}
		statuses = append(statuses, &pb.StrategyAssignmentStatus{
			Strategy:           stName,
			Phase:              phaseOut,
			RunningArtifact:    stSpec.GetArtifact(),
			RunningConfig:      stSpec.GetConfig(),
			ObservedGeneration: rec.Generation,
			Conditions: []*pb.Condition{
				{Type: "Live", Status: pb.ConditionStatus_CONDITION_STATUS_TRUE},
				{Type: "Ready", Status: readyOut},
			},
		})
	}
	if err := st.ApplyStatus(machine, &pb.StatusReport{
		ObservedGeneration: rec.Generation,
		Assignments:        statuses,
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
	spec := rec.Assignments[slotName("m1")]
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

	if _, _, err := st.ApplyAssignmentSet(&pb.AssignmentSet{
		Metadata: &pb.ObjectMeta{Name: "trading"},
		Spec: &pb.AssignmentSetSpec{
			ArtifactVersion: "v2",
			ConfigVersion:   "c1",
			Strategy:        "nats",
			Members:         loadCluster(t, st).GetSpec().GetMembers(),
			Update:          &pb.RollingUpdate{MaxUnavailable: 1, WaitReadySeconds: 30},
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
		if rec.Assignments[slotName(m)].GetArtifact().GetVersion() == "v2" {
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
	servers := cl.GetSpec().GetMembers()
	for i, srv := range servers {
		if i == 2 {
			break
		}
		spec, err := computeAssignment(cl, i, art, cfg)
		if err != nil {
			t.Fatal(err)
		}
		if _, _, err := asg.Apply(ctx, assign.Request{
			MachineID: srv.GetMachine(), Strategy: srv.GetName(), Spec: spec, AllowReserved: true,
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
	if _, _, err := st.SetAssignment("m1", slotName("m1"), nil); err != nil {
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

func TestNoConfigOmitsConfigArg(t *testing.T) {
	st := store.NewMemory(nil)
	if err := st.RegisterArtifact(&pb.ArtifactRef{
		Name: "nats", Version: "v1", Digest: "sha256:nats1", Uri: "file:///nats-v1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertMachine(&pb.Register{MachineId: "m1"}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ApplyAssignmentSet(&pb.AssignmentSet{
		Metadata: &pb.ObjectMeta{Name: "trading"},
		Spec: &pb.AssignmentSetSpec{
			ArtifactVersion: "v1",
			Strategy:        "nats",
			Members: []*pb.SetMember{{
				Machine: "m1", Name: "nats-m1",
				Vars: map[string]string{"route_host": "10.0.0.1", "cluster_port": "6222", "monitor_port": "8222"},
			}},
			Update: &pb.RollingUpdate{MaxUnavailable: 1, WaitReadySeconds: 30},
		},
	}); err != nil {
		t.Fatal(err)
	}
	ctrl := New(st, assign.New(st, nil), nil, nil)
	if err := ctrl.Reconcile(context.Background(), loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	rec, _ := st.GetMachine("m1")
	joined := strings.Join(rec.Assignments[slotName("m1")].GetArgs(), " ")
	if strings.Contains(joined, "${CONFIG}") || strings.Contains(joined, "-c") {
		t.Fatalf("args = %v, want no -c ${CONFIG}", rec.Assignments[slotName("m1")].GetArgs())
	}
}

func TestShrinkUndeploysDroppedMember(t *testing.T) {
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

	keep := loadCluster(t, st).GetSpec().GetMembers()[:2]
	if _, _, err := st.ApplyAssignmentSet(&pb.AssignmentSet{
		Metadata: &pb.ObjectMeta{Name: "trading"},
		Spec: &pb.AssignmentSetSpec{
			ArtifactVersion: "v1",
			ConfigVersion:   "c1",
			Strategy:        "nats",
			Members:         keep,
			Update:          &pb.RollingUpdate{MaxUnavailable: 1, WaitReadySeconds: 30},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	if assigned(st, "m3") {
		t.Fatal("dropped member m3 still assigned")
	}
	if !assigned(st, "m1") || !assigned(st, "m2") {
		t.Fatal("kept members must stay assigned")
	}
}

func TestAssignErrorWritesFailedStatus(t *testing.T) {
	st := store.NewMemory(nil)
	if err := st.RegisterArtifact(&pb.ArtifactRef{
		Name: "nats", Version: "v1", Digest: "sha256:nats1", Uri: "file:///nats-v1",
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := st.ApplyAssignmentSet(&pb.AssignmentSet{
		Metadata: &pb.ObjectMeta{Name: "trading"},
		Spec: &pb.AssignmentSetSpec{
			ArtifactVersion: "v1",
			Strategy:        "nats",
			Members: []*pb.SetMember{{
				Machine: "ghost", Name: "nats-ghost",
				Vars: map[string]string{"route_host": "10.0.0.9", "cluster_port": "6222", "monitor_port": "8222"},
			}},
			Update: &pb.RollingUpdate{MaxUnavailable: 1, WaitReadySeconds: 30},
		},
	}); err != nil {
		t.Fatal(err)
	}
	ctrl := New(st, assign.New(st, nil), nil, nil)
	if err := ctrl.Reconcile(context.Background(), loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	cl := loadCluster(t, st)
	if cl.GetStatus().GetPhase() != "Failed" || cl.GetStatus().GetReason() != "AssignFailed" {
		t.Fatalf("status = %+v", cl.GetStatus())
	}
	if !strings.Contains(cl.GetStatus().GetMessage(), "unknown machine") {
		t.Fatalf("message = %q", cl.GetStatus().GetMessage())
	}
}

func TestReadyMemberDeadlineDoesNotDegradeLaterFlap(t *testing.T) {
	ctrl, st, _ := setupCluster(t, 1)
	ctx := context.Background()
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	ctrl.Now = func() time.Time { return base }
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	markReady(t, st, "m1", true, pb.DeployPhase_DEPLOY_PHASE_HEALTHY)
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	if loadCluster(t, st).GetStatus().GetPhase() != "Ready" {
		t.Fatalf("phase=%s", loadCluster(t, st).GetStatus().GetPhase())
	}

	markReady(t, st, "m1", false, pb.DeployPhase_DEPLOY_PHASE_HEALTHY)
	ctrl.Now = func() time.Time { return base.Add(31 * time.Second) }
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	cl := loadCluster(t, st)
	if cl.GetStatus().GetPhase() == "Degraded" {
		t.Fatal("stale deadline must not Degrade after a Ready member flaps")
	}
}

func TestDeleteNeverAssignedRemovesCluster(t *testing.T) {
	ctrl, st, _ := setupCluster(t, 2)
	ctx := context.Background()
	if _, err := st.MarkAssignmentSetDeleting("trading"); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.GetAssignmentSet("trading"); ok {
		t.Fatal("never-assigned deleting cluster should be removed")
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

func TestReconcileUsesSpecIndexNotSortOrder(t *testing.T) {
	st := store.NewMemory(nil)
	registerNATSArtifacts(t, st)
	for _, id := range []string{"m1", "m2", "m3"} {
		if _, err := st.UpsertMachine(&pb.Register{MachineId: id}); err != nil {
			t.Fatal(err)
		}
	}
	if _, _, err := st.ApplyAssignmentSet(&pb.AssignmentSet{
		Metadata: &pb.ObjectMeta{Name: "trading"},
		Spec: &pb.AssignmentSetSpec{
			ArtifactVersion: "v1",
			ConfigVersion:   "c1",
			Strategy:        "nats",
			Template:        natsTemplate(),
			Members: []*pb.SetMember{
				{Machine: "m3", Name: "nats-m3", Vars: map[string]string{"route_host": "10.0.0.3", "cluster_port": "6222", "monitor_port": "8223"}},
				{Machine: "m1", Name: "nats-m1", Vars: map[string]string{"route_host": "10.0.0.1", "cluster_port": "6222", "monitor_port": "8221"}},
				{Machine: "m2", Name: "nats-m2", Vars: map[string]string{"route_host": "10.0.0.2", "cluster_port": "6222", "monitor_port": "8222"}},
			},
			Update: &pb.RollingUpdate{MaxUnavailable: 1, WaitReadySeconds: 30},
		},
	}); err != nil {
		t.Fatal(err)
	}
	ctrl := New(st, assign.New(st, nil), nil, nil)
	if err := ctrl.Reconcile(context.Background(), loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	rec, _ := st.GetMachine("m1")
	spec := rec.Assignments["nats-m1"]
	if spec == nil {
		t.Fatal("m1 should receive the nats-m1 slot, not a sorted-index neighbor")
	}
	if spec.GetEnv()["NATS_SERVER_NAME"] != "nats-m1" {
		t.Fatalf("NATS_SERVER_NAME=%q", spec.GetEnv()["NATS_SERVER_NAME"])
	}
	if spec.GetReadiness().GetEndpoint() != "http://127.0.0.1:8221/healthz" {
		t.Fatalf("endpoint=%q", spec.GetReadiness().GetEndpoint())
	}
}

func TestSameHostMembersRollInStableNameOrder(t *testing.T) {
	ctrl, st := setupSameHost(t, []string{"nats-z", "nats-a"})
	ctx := context.Background()
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	rec, _ := st.GetMachine("m1")
	if rec.Assignments["nats-a"] == nil || rec.Assignments["nats-z"] != nil {
		t.Fatalf("first write should be nats-a, got %v", assignmentNames(rec))
	}
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	rec, _ = st.GetMachine("m1")
	if rec.Assignments["nats-z"] != nil {
		t.Fatal("second reconcile must not flip to nats-z while nats-a is in-flight")
	}
}

func TestSameHostThreeMembersOneAtATime(t *testing.T) {
	ctrl, st := setupSameHost(t, []string{"nats-a", "nats-b", "nats-c"})
	ctx := context.Background()
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	rec, _ := st.GetMachine("m1")
	if rec.Assignments["nats-a"] == nil || rec.Assignments["nats-b"] != nil || rec.Assignments["nats-c"] != nil {
		t.Fatalf("first pass: %v", assignmentNames(rec))
	}
	markReadySlot(t, st, "m1", "nats-a", true, pb.DeployPhase_DEPLOY_PHASE_HEALTHY)
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	rec, _ = st.GetMachine("m1")
	if rec.Assignments["nats-b"] == nil || rec.Assignments["nats-c"] != nil {
		t.Fatalf("second pass: %v", assignmentNames(rec))
	}
}

func TestSameHostDropOneMemberLeavesTheOther(t *testing.T) {
	ctrl, st := setupSameHost(t, []string{"nats-a", "nats-b"})
	ctx := context.Background()
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	markReadySlot(t, st, "m1", "nats-a", true, pb.DeployPhase_DEPLOY_PHASE_HEALTHY)
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	markReadySlot(t, st, "m1", "nats-b", true, pb.DeployPhase_DEPLOY_PHASE_HEALTHY)
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}

	keep := []*pb.SetMember{{
		Machine: "m1", Name: "nats-a",
		Vars: map[string]string{"route_host": "127.0.0.1", "cluster_port": "6222", "monitor_port": "8222"},
	}}
	if _, _, err := st.ApplyAssignmentSet(&pb.AssignmentSet{
		Metadata: &pb.ObjectMeta{Name: "trading"},
		Spec: &pb.AssignmentSetSpec{
			ArtifactVersion: "v1", ConfigVersion: "c1", Strategy: "nats",
			Template: natsTemplate(), Members: keep,
			Update: &pb.RollingUpdate{MaxUnavailable: 1, WaitReadySeconds: 30},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	rec, _ := st.GetMachine("m1")
	if rec.Assignments["nats-a"] == nil {
		t.Fatal("kept member undeployed")
	}
	if rec.Assignments["nats-b"] != nil {
		t.Fatal("dropped same-host member still assigned")
	}
}

func TestLegacyMigrateKeepsQuorum(t *testing.T) {
	ctrl, st, _ := setupCluster(t, 3)
	ctx := context.Background()
	cl := loadCluster(t, st)
	seedLegacyFamily(t, st, cl)

	if liveProcessCount(st, "m1", "m2", "m3") != 3 {
		t.Fatal("seed")
	}
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	if n := liveProcessCount(st, "m1", "m2", "m3"); n < 2 {
		t.Fatalf("after first migrate live=%d, want >= 2", n)
	}
	if !assignedSlot(st, "m1", "nats-m1") || assignedSlot(st, "m1", "nats") {
		t.Fatal("m1 should have swapped nats → nats-m1 in one tick")
	}
	if !assignedSlot(st, "m2", "nats") || assignedSlot(st, "m2", "nats-m2") {
		t.Fatal("m2 must still be on the family slot")
	}

	markReady(t, st, "m1", true, pb.DeployPhase_DEPLOY_PHASE_HEALTHY)
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	if n := liveProcessCount(st, "m1", "m2", "m3"); n < 2 {
		t.Fatalf("after second migrate live=%d", n)
	}
}

func TestDeleteUnmigratedSetDrainsFamilySlots(t *testing.T) {
	ctrl, st, _ := setupCluster(t, 3)
	ctx := context.Background()
	seedLegacyFamily(t, st, loadCluster(t, st))
	if _, err := st.MarkAssignmentSetDeleting("trading"); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	if _, ok := st.GetAssignmentSet("trading"); !ok {
		t.Fatal("must not delete the row while family slots remain")
	}
	if liveProcessCount(st, "m1", "m2", "m3") != 2 {
		t.Fatalf("delete should undeploy one family slot, live=%d", liveProcessCount(st, "m1", "m2", "m3"))
	}
	for i := 0; i < 3; i++ {
		cl, ok := st.GetAssignmentSet("trading")
		if !ok {
			break
		}
		if err := ctrl.Reconcile(ctx, cl); err != nil {
			t.Fatal(err)
		}
	}
	if _, ok := st.GetAssignmentSet("trading"); ok {
		t.Fatal("set row should be gone after family slots drain")
	}
	if liveProcessCount(st, "m1", "m2", "m3") != 0 {
		t.Fatal("legacy nats assignments leaked after delete")
	}
}

func TestLegacyShrinkAfterPartialMigrateDoesNotOrphanFamilySlot(t *testing.T) {
	ctrl, st, _ := setupCluster(t, 3)
	ctx := context.Background()
	seedLegacyFamily(t, st, loadCluster(t, st))

	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	if !assignedSlot(st, "m1", "nats-m1") || assignedSlot(st, "m1", "nats") {
		t.Fatal("first tick should pair-migrate m1")
	}
	markReady(t, st, "m1", true, pb.DeployPhase_DEPLOY_PHASE_HEALTHY)

	cur := loadCluster(t, st)
	keep := keepMembers(cur, "m1")
	if _, _, err := st.ApplyAssignmentSet(&pb.AssignmentSet{
		Metadata: &pb.ObjectMeta{Name: "trading"},
		Spec: &pb.AssignmentSetSpec{
			ArtifactVersion: cur.GetSpec().GetArtifactVersion(),
			ConfigVersion:   cur.GetSpec().GetConfigVersion(),
			Strategy:        cur.GetSpec().GetStrategy(),
			Template:        cur.GetSpec().GetTemplate(),
			Members:         keep,
			Update:          cur.GetSpec().GetUpdate(),
		},
	}); err != nil {
		t.Fatal(err)
	}

	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	if loadCluster(t, st).GetStatus().GetAssignmentKey() == store.AssignmentKeyMember {
		t.Fatal("must not flip assignment_key while a dropped machine still holds nats")
	}
	if assignedSlot(st, "m2", "nats") {
		t.Fatal("first shrink tick should undeploy one leftover family slot")
	}
	if !assignedSlot(st, "m3", "nats") {
		t.Fatal("maxUnavailable:1 must leave m3 nats assigned after the first shrink tick")
	}
	if !statusHasMember(loadCluster(t, st), "m3", "nats") {
		t.Fatal("status must retain the leftover family drop so the next tick can see it")
	}

	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	if assignedSlot(st, "m3", "nats") {
		t.Fatal("second tick must undeploy leftover nats on m3")
	}
	if !assignedSlot(st, "m1", "nats-m1") {
		t.Fatal("kept member lost")
	}
	if loadCluster(t, st).GetStatus().GetAssignmentKey() != store.AssignmentKeyMember {
		t.Fatalf("assignment_key=%q after leftover drain", loadCluster(t, st).GetStatus().GetAssignmentKey())
	}
}

func TestShrinkDropsExceedingMaxUnavailableEventuallyDrainsAll(t *testing.T) {
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
	if loadCluster(t, st).GetStatus().GetAssignmentKey() != store.AssignmentKeyMember {
		t.Fatalf("assignment_key=%q", loadCluster(t, st).GetStatus().GetAssignmentKey())
	}

	cur := loadCluster(t, st)
	if _, _, err := st.ApplyAssignmentSet(&pb.AssignmentSet{
		Metadata: &pb.ObjectMeta{Name: "trading"},
		Spec: &pb.AssignmentSetSpec{
			ArtifactVersion: cur.GetSpec().GetArtifactVersion(),
			ConfigVersion:   cur.GetSpec().GetConfigVersion(),
			Strategy:        cur.GetSpec().GetStrategy(),
			Template:        cur.GetSpec().GetTemplate(),
			Members:         keepMembers(cur, "m1"),
			Update:          cur.GetSpec().GetUpdate(),
		},
	}); err != nil {
		t.Fatal(err)
	}

	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	if assigned(st, "m2") {
		t.Fatal("first shrink tick should undeploy one dropped member")
	}
	if !assigned(st, "m3") {
		t.Fatal("maxUnavailable:1 must leave the second drop assigned")
	}
	if !statusHasMember(loadCluster(t, st), "m3", slotName("m3")) {
		t.Fatal("status must retain the extra drop across the partial undeploy")
	}

	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	if assigned(st, "m2") || assigned(st, "m3") {
		t.Fatalf("both drops must be undeployed, m2=%v m3=%v", assigned(st, "m2"), assigned(st, "m3"))
	}
	if !assigned(st, "m1") {
		t.Fatal("kept member lost")
	}
}

func TestAssignmentKeyFlipStopsFamilyDrain(t *testing.T) {
	ctrl, st, _ := setupCluster(t, 1)
	ctx := context.Background()
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	markReady(t, st, "m1", true, pb.DeployPhase_DEPLOY_PHASE_HEALTHY)
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	if loadCluster(t, st).GetStatus().GetAssignmentKey() != store.AssignmentKeyMember {
		t.Fatalf("assignment_key=%q", loadCluster(t, st).GetStatus().GetAssignmentKey())
	}
	if _, _, err := st.SetAssignment("m1", "nats", &pb.StrategyAssignmentSpec{
		Strategy: "nats", Artifact: &pb.ArtifactRef{Name: "nats", Version: "v1", Digest: "sha256:nats1"},
	}); err != nil {
		t.Fatal(err)
	}
	if err := ctrl.Reconcile(ctx, loadCluster(t, st)); err != nil {
		t.Fatal(err)
	}
	if !assignedSlot(st, "m1", "nats") {
		t.Fatal("after flip, a human nats assignment must not be auto-undeployed")
	}
	if !assignedSlot(st, "m1", "nats-m1") {
		t.Fatal("member slot disappeared")
	}
}

func registerNATSArtifacts(t *testing.T, st *store.Memory) {
	t.Helper()
	for _, ref := range []*pb.ArtifactRef{
		{Name: "nats", Version: "v1", Digest: "sha256:nats1", Uri: "file:///nats-v1"},
		{Name: "nats-config", Version: "c1", Digest: "sha256:cfg1", Uri: "file:///nats.conf"},
		{Name: "nats", Version: "v2", Digest: "sha256:nats2", Uri: "file:///nats-v2"},
	} {
		if err := st.RegisterArtifact(ref); err != nil {
			t.Fatal(err)
		}
	}
}

func setupSameHost(t *testing.T, names []string) (*Controller, *store.Memory) {
	t.Helper()
	st := store.NewMemory(nil)
	registerNATSArtifacts(t, st)
	if _, err := st.UpsertMachine(&pb.Register{MachineId: "m1"}); err != nil {
		t.Fatal(err)
	}
	members := make([]*pb.SetMember, 0, len(names))
	for i, name := range names {
		port := 8222 + i
		members = append(members, &pb.SetMember{
			Machine: "m1", Name: name,
			Vars: map[string]string{
				"route_host": "127.0.0.1", "cluster_port": strconv.Itoa(6222 + i), "monitor_port": strconv.Itoa(port),
			},
		})
	}
	if _, _, err := st.ApplyAssignmentSet(&pb.AssignmentSet{
		Metadata: &pb.ObjectMeta{Name: "trading"},
		Spec: &pb.AssignmentSetSpec{
			ArtifactVersion: "v1", ConfigVersion: "c1", Strategy: "nats",
			Template: natsTemplate(), Members: members,
			Update: &pb.RollingUpdate{MaxUnavailable: 1, WaitReadySeconds: 30},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return New(st, assign.New(st, nil), nil, nil), st
}

func keepMembers(cl *pb.AssignmentSet, machines ...string) []*pb.SetMember {
	want := map[string]struct{}{}
	for _, m := range machines {
		want[m] = struct{}{}
	}
	var out []*pb.SetMember
	for _, srv := range cl.GetSpec().GetMembers() {
		if _, ok := want[srv.GetMachine()]; !ok {
			continue
		}
		out = append(out, proto.Clone(srv).(*pb.SetMember))
	}
	return out
}

func statusHasMember(cl *pb.AssignmentSet, machine, name string) bool {
	for _, m := range cl.GetStatus().GetMembers() {
		if m.GetMachine() == machine && m.GetName() == name {
			return true
		}
	}
	return false
}

func seedLegacyFamily(t *testing.T, st store.Store, cl *pb.AssignmentSet) {
	t.Helper()
	art, cfg, err := resolveClusterArtifacts(st, cl)
	if err != nil {
		t.Fatal(err)
	}
	for i, srv := range cl.GetSpec().GetMembers() {
		spec, err := computeAssignment(cl, i, art, cfg)
		if err != nil {
			t.Fatal(err)
		}
		spec = proto.Clone(spec).(*pb.StrategyAssignmentSpec)
		spec.Strategy = "nats"
		if _, _, err := st.SetAssignment(srv.GetMachine(), "nats", spec); err != nil {
			t.Fatal(err)
		}
	}
}

func liveProcessCount(st store.Store, machines ...string) int {
	n := 0
	for _, m := range machines {
		rec, ok := st.GetMachine(m)
		if !ok {
			continue
		}
		for name := range rec.Assignments {
			if name == "nats" || strings.HasPrefix(name, "nats-") {
				n++
			}
		}
	}
	return n
}

func assignmentNames(rec *store.MachineRecord) []string {
	var out []string
	for n := range rec.Assignments {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}
