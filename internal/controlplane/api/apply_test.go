package api

import (
	"context"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/controlplane/store"
)

func TestApplyAssignmentHonoursStoppedAndNoPrune(t *testing.T) {
	client, st, _, agents := startHumanAPI(t)
	ctx := context.Background()
	st.UpsertMachine(&pb.Register{MachineId: "m1"})
	client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "hello", Version: "v1", Digest: "sha256:aaa", Uri: "file:///a"},
	}))
	client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "other", Version: "v1", Digest: "sha256:bbb", Uri: "file:///b"},
	}))
	if _, err := client.Deploy(ctx, connect.NewRequest(&pb.DeployRequest{
		MachineId: "m1", Strategy: "other", ArtifactVersion: "v1",
	})); err != nil {
		t.Fatal(err)
	}

	n := agents.n
	resp, err := client.ApplyAssignment(ctx, connect.NewRequest(&pb.ApplyAssignmentRequest{
		MachineId:       "m1",
		Strategy:        "hello",
		ArtifactVersion: "v1",
		Stopped:         false,
		Env:             map[string]string{"A": "1"},
		Args:            []string{"-c", "${CONFIG}"},
		Schedules: []*pb.CronSchedule{{
			Name: "hourly", CronExpr: "0 * * * *", Timezone: "UTC",
			Action: pb.CronAction_CRON_ACTION_RESTART,
		}},
		DeployPolicy: &pb.DeployPolicy{HealthWindowSeconds: 45, EnableAutoRollback: true},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.GetGeneration() < 1 {
		t.Fatal("expected generation bump")
	}
	if agents.n != n+1 {
		t.Fatalf("notify=%d", agents.n)
	}
	rec, _ := st.GetMachine("m1")
	hello := rec.Assignments["hello"]
	if hello.GetStopped() {
		t.Fatal("Apply with stopped=false must start")
	}
	if hello.GetEnv()["A"] != "1" || hello.GetDeployPolicy().GetHealthWindowSeconds() != 45 {
		t.Fatalf("spec = %+v", hello)
	}
	if rec.Assignments["other"] == nil {
		t.Fatal("other strategy was pruned")
	}

	n = agents.n
	gen := resp.Msg.GetGeneration()
	again, err := client.ApplyAssignment(ctx, connect.NewRequest(&pb.ApplyAssignmentRequest{
		MachineId:       "m1",
		Strategy:        "hello",
		ArtifactVersion: "v1",
		Stopped:         false,
		Env:             map[string]string{"A": "1"},
		Args:            []string{"-c", "${CONFIG}"},
		Schedules: []*pb.CronSchedule{{
			Name: "hourly", CronExpr: "0 * * * *", Timezone: "UTC",
			Action: pb.CronAction_CRON_ACTION_RESTART,
		}},
		DeployPolicy: &pb.DeployPolicy{HealthWindowSeconds: 45, EnableAutoRollback: true},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if again.Msg.GetGeneration() != gen || agents.n != n {
		t.Fatal("identical ApplyAssignment should no-op")
	}

	// Deploy still create-then-start.
	client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "fresh", Version: "v1", Digest: "sha256:ccc", Uri: "file:///c"},
	}))
	if _, err := client.Deploy(ctx, connect.NewRequest(&pb.DeployRequest{
		MachineId: "m1", Strategy: "fresh", ArtifactVersion: "v1",
	})); err != nil {
		t.Fatal(err)
	}
	rec, _ = st.GetMachine("m1")
	if !rec.Assignments["fresh"].GetStopped() {
		t.Fatal("Deploy on a new strategy must land stopped")
	}
}

func TestApplyAssignmentErrorClasses(t *testing.T) {
	client, st, _, _ := startHumanAPI(t)
	ctx := context.Background()
	_, err := client.ApplyAssignment(ctx, connect.NewRequest(&pb.ApplyAssignmentRequest{
		MachineId: "missing", Strategy: "s", ArtifactVersion: "v1",
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("missing machine: %v", err)
	}
	st.UpsertMachine(&pb.Register{MachineId: "m1"})
	_, err = client.ApplyAssignment(ctx, connect.NewRequest(&pb.ApplyAssignmentRequest{
		MachineId: "m1", Strategy: "s", ArtifactVersion: "nope",
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown version: %v", err)
	}

	srv := New(st, nil, nil, nil)
	_ = st.RegisterArtifactRecord(&store.ArtifactRecord{
		Ref:   &pb.ArtifactRef{Name: "s", Version: "v1", Digest: "sha256:aaa", Uri: "https://example.com/bin"},
		State: store.ArtifactStatePending,
	})
	_, err = srv.ApplyAssignment(ctx, connect.NewRequest(&pb.ApplyAssignmentRequest{
		MachineId: "m1", Strategy: "s", ArtifactVersion: "v1",
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition || !strings.Contains(err.Error(), "PENDING") {
		t.Fatalf("pending: %v", err)
	}
}

func TestApplyAssignmentSetPersistOnlyAndReservation(t *testing.T) {
	client, st, _, _ := startHumanAPI(t)
	ctx := context.Background()
	st.UpsertMachine(&pb.Register{MachineId: "m1"})
	st.UpsertMachine(&pb.Register{MachineId: "m2"})
	client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "nats", Version: "v1", Digest: "sha256:nats", Uri: "file:///nats"},
	}))

	resp, err := client.ApplyAssignmentSet(ctx, connect.NewRequest(&pb.ApplyAssignmentSetRequest{
		Set: &pb.AssignmentSet{
			Metadata: &pb.ObjectMeta{Name: "trading"},
			Spec: &pb.AssignmentSetSpec{
				Strategy:        "nats",
				ArtifactVersion: "v1",
				Members: []*pb.SetMember{
					{Machine: "m1", Name: "n1", Vars: map[string]string{"route_host": "10.0.0.1", "cluster_port": "6222", "monitor_port": "8222"}},
					{Machine: "m2", Name: "n2", Vars: map[string]string{"route_host": "10.0.0.2", "cluster_port": "6222", "monitor_port": "8222"}},
				},
			},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	c := resp.Msg.GetSet()
	if c.GetMetadata().GetUid() == "" || c.GetMetadata().GetGeneration() != 1 || c.GetStatus().GetPhase() != "Pending" {
		t.Fatalf("cluster = %+v", c)
	}
	rec, _ := st.GetMachine("m1")
	if rec.Assignments["nats"] != nil {
		t.Fatal("ApplyAssignmentSet must not write assignments")
	}

	// Human deploy to an owned slot is rejected.
	_, err = client.Deploy(ctx, connect.NewRequest(&pb.DeployRequest{
		MachineId: "m1", Strategy: "nats", ArtifactVersion: "v1",
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("owned deploy: %v", err)
	}

	// A template referencing a var the member does not declare is the generic
	// replacement for the old "empty routeHost" check: the control plane has no
	// opinion about route hosts, but it will not write a spec it cannot render.
	st.UpsertMachine(&pb.Register{MachineId: "m9"})
	_, err = client.ApplyAssignmentSet(ctx, connect.NewRequest(&pb.ApplyAssignmentSetRequest{
		Set: &pb.AssignmentSet{
			Metadata: &pb.ObjectMeta{Name: "bad"},
			Spec: &pb.AssignmentSetSpec{
				Strategy:        "nats",
				ArtifactVersion: "v1",
				Template: &pb.MemberTemplate{
					Args: []string{"--host", "${member.vars.route_host}"},
				},
				Members: []*pb.SetMember{{Machine: "m9", Name: "x"}},
			},
		},
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("unknown placeholder: %v", err)
	}
	if _, ok := st.GetAssignmentSet("bad"); ok {
		t.Fatal("rejected apply wrote a row")
	}

	// Unowned existing assignment blocks apply.
	st.UpsertMachine(&pb.Register{MachineId: "m3"})
	if _, _, err := st.SetAssignment("m3", "nats", &pb.StrategyAssignmentSpec{
		Strategy: "nats", Artifact: &pb.ArtifactRef{Version: "v1", Digest: "sha256:nats"},
	}); err != nil {
		t.Fatal(err)
	}
	_, err = client.ApplyAssignmentSet(ctx, connect.NewRequest(&pb.ApplyAssignmentSetRequest{
		Set: &pb.AssignmentSet{
			Metadata: &pb.ObjectMeta{Name: "other"},
			Spec: &pb.AssignmentSetSpec{
				Strategy:        "nats",
				ArtifactVersion: "v1",
				Members:         []*pb.SetMember{{Machine: "m3", Name: "n3", Vars: map[string]string{"route_host": "10.0.0.3", "cluster_port": "6222", "monitor_port": "8222"}}},
			},
		},
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("unowned assignment: %v", err)
	}
	if _, ok := st.GetAssignmentSet("other"); ok {
		t.Fatal("rejected apply wrote a row")
	}

	got, err := client.GetAssignmentSet(ctx, connect.NewRequest(&pb.GetAssignmentSetRequest{Name: "trading"}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Msg.GetMetadata().GetName() != "trading" {
		t.Fatalf("get = %+v", got.Msg)
	}
	list, err := client.ListAssignmentSets(ctx, connect.NewRequest(&pb.ListAssignmentSetsRequest{}))
	if err != nil || len(list.Msg.GetSets()) != 1 {
		t.Fatalf("list err=%v n=%d", err, len(list.Msg.GetSets()))
	}
}

func TestApplyAssignmentSetRejectsOverlappingMembership(t *testing.T) {
	client, st, _, _ := startHumanAPI(t)
	ctx := context.Background()
	st.UpsertMachine(&pb.Register{MachineId: "m1"})
	client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "nats", Version: "v1", Digest: "sha256:nats", Uri: "file:///nats"},
	}))

	if _, err := client.ApplyAssignmentSet(ctx, connect.NewRequest(&pb.ApplyAssignmentSetRequest{
		Set: &pb.AssignmentSet{
			Metadata: &pb.ObjectMeta{Name: "alpha"},
			Spec: &pb.AssignmentSetSpec{
				Strategy:        "nats",
				ArtifactVersion: "v1",
				Members:         []*pb.SetMember{{Machine: "m1", Name: "n1", Vars: map[string]string{"route_host": "10.0.0.1", "cluster_port": "6222", "monitor_port": "8222"}}},
			},
		},
	})); err != nil {
		t.Fatal(err)
	}

	_, err := client.ApplyAssignmentSet(ctx, connect.NewRequest(&pb.ApplyAssignmentSetRequest{
		Set: &pb.AssignmentSet{
			Metadata: &pb.ObjectMeta{Name: "beta"},
			Spec: &pb.AssignmentSetSpec{
				Strategy:        "nats",
				ArtifactVersion: "v1",
				Members:         []*pb.SetMember{{Machine: "m1", Name: "n2", Vars: map[string]string{"route_host": "10.0.0.1", "cluster_port": "6222", "monitor_port": "8222"}}},
			},
		},
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("overlapping apply: %v", err)
	}
	if _, ok := st.GetAssignmentSet("beta"); ok {
		t.Fatal("rejected overlapping apply wrote a row")
	}
}

func TestApplyAssignmentExplicitArtifact(t *testing.T) {
	client, st, _, _ := startHumanAPI(t)
	ctx := context.Background()
	st.UpsertMachine(&pb.Register{MachineId: "m1"})
	if _, err := client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "nats", Version: "v1", Digest: "sha256:nats", Uri: "file:///nats"},
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ApplyAssignment(ctx, connect.NewRequest(&pb.ApplyAssignmentRequest{
		MachineId:       "m1",
		Strategy:        "nats-m4",
		Artifact:        "nats",
		ArtifactVersion: "v1",
		Stopped:         false,
	})); err != nil {
		t.Fatal(err)
	}
	rec, _ := st.GetMachine("m1")
	got := rec.Assignments["nats-m4"]
	if got == nil || got.GetArtifact().GetName() != "nats" {
		t.Fatalf("assignment = %+v", got)
	}
}

func TestApplyAssignmentSetTrimsMemberName(t *testing.T) {
	client, st, _, _ := startHumanAPI(t)
	ctx := context.Background()
	st.UpsertMachine(&pb.Register{MachineId: "m1"})
	if _, err := client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "nats", Version: "v1", Digest: "sha256:nats", Uri: "file:///nats"},
	})); err != nil {
		t.Fatal(err)
	}
	got, err := client.ApplyAssignmentSet(ctx, connect.NewRequest(&pb.ApplyAssignmentSetRequest{
		Set: &pb.AssignmentSet{
			Metadata: &pb.ObjectMeta{Name: "trading"},
			Spec: &pb.AssignmentSetSpec{
				Strategy:        "nats",
				ArtifactVersion: "v1",
				Members: []*pb.SetMember{{
					Machine: "m1", Name: "  nats-m1  ",
					Vars: map[string]string{"route_host": "10.0.0.1", "cluster_port": "6222", "monitor_port": "8222"},
				}},
			},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if name := got.Msg.GetSet().GetSpec().GetMembers()[0].GetName(); name != "nats-m1" {
		t.Fatalf("stored name = %q, want trimmed nats-m1", name)
	}
}

func TestApplyAssignmentSetRejectsBadMemberName(t *testing.T) {
	client, st, _, _ := startHumanAPI(t)
	ctx := context.Background()
	st.UpsertMachine(&pb.Register{MachineId: "m1"})
	if _, err := client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "nats", Version: "v1", Digest: "sha256:nats", Uri: "file:///nats"},
	})); err != nil {
		t.Fatal(err)
	}
	_, err := client.ApplyAssignmentSet(ctx, connect.NewRequest(&pb.ApplyAssignmentSetRequest{
		Set: &pb.AssignmentSet{
			Metadata: &pb.ObjectMeta{Name: "trading"},
			Spec: &pb.AssignmentSetSpec{
				Strategy:        "nats",
				ArtifactVersion: "v1",
				Members:         []*pb.SetMember{{Machine: "m1", Name: "nats/m1", Vars: map[string]string{"route_host": "10.0.0.1"}}},
			},
		},
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad member name: %v", err)
	}

	_, err = client.ApplyAssignmentSet(ctx, connect.NewRequest(&pb.ApplyAssignmentSetRequest{
		Set: &pb.AssignmentSet{
			Metadata: &pb.ObjectMeta{Name: "dot"},
			Spec: &pb.AssignmentSetSpec{
				Strategy:        "nats",
				ArtifactVersion: "v1",
				Members:         []*pb.SetMember{{Machine: "m1", Name: ".", Vars: map[string]string{"route_host": "10.0.0.1"}}},
			},
		},
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("dot member name: %v", err)
	}
}

func TestApplyAssignmentSetRejectsOCIOnExecOnlyMachine(t *testing.T) {
	client, st, _, _ := startHumanAPI(t)
	ctx := context.Background()
	if _, err := st.UpsertMachine(&pb.Register{
		MachineId: "m1",
		Spec: &pb.MachineSpec{
			SupportedDrivers: []pb.ExecutionDriver{pb.ExecutionDriver_EXECUTION_DRIVER_EXEC},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{
			Name: "nats", Version: "v1", Digest: "sha256:nats", Uri: "file:///nats.tar",
			Type: pb.ArtifactType_ARTIFACT_TYPE_OCI_IMAGE,
		},
	})); err != nil {
		t.Fatal(err)
	}
	_, err := client.ApplyAssignmentSet(ctx, connect.NewRequest(&pb.ApplyAssignmentSetRequest{
		Set: &pb.AssignmentSet{
			Metadata: &pb.ObjectMeta{Name: "trading"},
			Spec: &pb.AssignmentSetSpec{
				Strategy:        "nats",
				ArtifactVersion: "v1",
				Members:         []*pb.SetMember{{Machine: "m1", Name: "n1", Vars: map[string]string{"route_host": "10.0.0.1", "cluster_port": "6222", "monitor_port": "8222"}}},
			},
		},
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("oci admission: %v", err)
	}
	if _, ok := st.GetAssignmentSet("trading"); ok {
		t.Fatal("rejected OCI apply wrote a row")
	}
}
