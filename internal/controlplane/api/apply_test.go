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

func TestApplyNatsClusterPersistOnlyAndReservation(t *testing.T) {
	client, st, _, _ := startHumanAPI(t)
	ctx := context.Background()
	st.UpsertMachine(&pb.Register{MachineId: "m1"})
	st.UpsertMachine(&pb.Register{MachineId: "m2"})
	client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "nats", Version: "v1", Digest: "sha256:nats", Uri: "file:///nats"},
	}))

	resp, err := client.ApplyNatsCluster(ctx, connect.NewRequest(&pb.ApplyNatsClusterRequest{
		Cluster: &pb.NatsCluster{
			Metadata: &pb.ObjectMeta{Name: "trading"},
			Spec: &pb.NatsClusterSpec{
				ArtifactVersion: "v1",
				Servers: []*pb.NatsServer{
					{Machine: "m1", ServerName: "n1", RouteHost: "10.0.0.1"},
					{Machine: "m2", ServerName: "n2", RouteHost: "10.0.0.2"},
				},
			},
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	c := resp.Msg.GetCluster()
	if c.GetMetadata().GetUid() == "" || c.GetMetadata().GetGeneration() != 1 || c.GetStatus().GetPhase() != "Pending" {
		t.Fatalf("cluster = %+v", c)
	}
	rec, _ := st.GetMachine("m1")
	if rec.Assignments["nats"] != nil {
		t.Fatal("ApplyNatsCluster must not write assignments")
	}

	// Human deploy to an owned slot is rejected.
	_, err = client.Deploy(ctx, connect.NewRequest(&pb.DeployRequest{
		MachineId: "m1", Strategy: "nats", ArtifactVersion: "v1",
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("owned deploy: %v", err)
	}

	// Invalid routeHost.
	_, err = client.ApplyNatsCluster(ctx, connect.NewRequest(&pb.ApplyNatsClusterRequest{
		Cluster: &pb.NatsCluster{
			Metadata: &pb.ObjectMeta{Name: "bad"},
			Spec: &pb.NatsClusterSpec{
				ArtifactVersion: "v1",
				Servers:         []*pb.NatsServer{{Machine: "m1", ServerName: "x", RouteHost: ""}},
			},
		},
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty route_host: %v", err)
	}

	// Unowned existing assignment blocks apply.
	st.UpsertMachine(&pb.Register{MachineId: "m3"})
	if _, _, err := st.SetAssignment("m3", "nats", &pb.StrategyAssignmentSpec{
		Strategy: "nats", Artifact: &pb.ArtifactRef{Version: "v1", Digest: "sha256:nats"},
	}); err != nil {
		t.Fatal(err)
	}
	_, err = client.ApplyNatsCluster(ctx, connect.NewRequest(&pb.ApplyNatsClusterRequest{
		Cluster: &pb.NatsCluster{
			Metadata: &pb.ObjectMeta{Name: "other"},
			Spec: &pb.NatsClusterSpec{
				ArtifactVersion: "v1",
				Servers:         []*pb.NatsServer{{Machine: "m3", ServerName: "n3", RouteHost: "10.0.0.3"}},
			},
		},
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("unowned assignment: %v", err)
	}
	if _, ok := st.GetNatsCluster("other"); ok {
		t.Fatal("rejected apply wrote a row")
	}

	got, err := client.GetNatsCluster(ctx, connect.NewRequest(&pb.GetNatsClusterRequest{Name: "trading"}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Msg.GetMetadata().GetName() != "trading" {
		t.Fatalf("get = %+v", got.Msg)
	}
	list, err := client.ListNatsClusters(ctx, connect.NewRequest(&pb.ListNatsClustersRequest{}))
	if err != nil || len(list.Msg.GetClusters()) != 1 {
		t.Fatalf("list err=%v n=%d", err, len(list.Msg.GetClusters()))
	}
}
