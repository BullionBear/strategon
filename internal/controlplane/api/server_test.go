package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/gen/strategyplatform/v1/strategyplatformv1connect"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

type stubAgents struct{ n int }

func (s *stubAgents) Notify(string) { s.n++ }

func startHumanAPI(t *testing.T) (strategyplatformv1connect.ControlPlaneServiceClient, store.Store, *store.Hub, *stubAgents) {
	t.Helper()
	hub := store.NewHub()
	st := store.NewMemory(hub)
	agents := &stubAgents{}
	srv := New(st, hub, agents, nil)
	mux := http.NewServeMux()
	path, h := strategyplatformv1connect.NewControlPlaneServiceHandler(srv)
	mux.Handle(path, h)
	ts := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	ts.Start()
	t.Cleanup(ts.Close)
	client := strategyplatformv1connect.NewControlPlaneServiceClient(http.DefaultClient, ts.URL)
	return client, st, hub, agents
}

func TestDeployJoinAndWatch(t *testing.T) {
	client, st, _, agents := startHumanAPI(t)
	ctx := context.Background()

	if _, err := st.UpsertMachine(&pb.Register{MachineId: "m1", Hostname: "host1", AgentVersion: 1}); err != nil {
		t.Fatal(err)
	}
	_, err := client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "s", Version: "v1", Digest: "sha256:aaa", Uri: "file:///tmp/s-v1"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "s", Version: "v2", Digest: "sha256:bbb", Uri: "file:///tmp/s-v2"},
	}))
	if err != nil {
		t.Fatal(err)
	}

	deployResp, err := client.Deploy(ctx, connect.NewRequest(&pb.DeployRequest{
		MachineId: "m1", Strategy: "s", ArtifactVersion: "v1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if deployResp.Msg.GetGeneration() != 1 {
		t.Fatalf("generation = %d, want 1", deployResp.Msg.GetGeneration())
	}
	if agents.n != 1 {
		t.Fatalf("agent notify count = %d, want 1", agents.n)
	}

	// Simulate agent status: still converging (running nothing yet).
	_ = st.ApplyStatus("m1", &pb.StatusReport{
		ObservedGeneration: 0,
		Assignments: []*pb.StrategyAssignmentStatus{{
			Strategy: "s", Phase: pb.DeployPhase_DEPLOY_PHASE_DOWNLOADING,
			ObservedGeneration: 1,
		}},
	})

	got, err := client.GetMachine(ctx, connect.NewRequest(&pb.GetMachineRequest{MachineId: "m1"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Msg.GetStrategies()) != 1 {
		t.Fatalf("strategies = %d, want 1", len(got.Msg.GetStrategies()))
	}
	sv := got.Msg.GetStrategies()[0]
	if sv.GetConverged() {
		t.Fatalf("should not be converged while DOWNLOADING")
	}
	if sv.GetDesiredArtifact().GetVersion() != "v1" || sv.GetPhase() != pb.DeployPhase_DEPLOY_PHASE_DOWNLOADING {
		t.Fatalf("unexpected view: %+v", sv)
	}

	// Converge: same digest + HEALTHY.
	_ = st.ApplyStatus("m1", &pb.StatusReport{
		ObservedGeneration: 1,
		Assignments: []*pb.StrategyAssignmentStatus{{
			Strategy: "s", Phase: pb.DeployPhase_DEPLOY_PHASE_HEALTHY,
			ObservedGeneration: 1,
			RunningArtifact:    &pb.ArtifactRef{Name: "s", Version: "v1", Digest: "sha256:aaa"},
			Pid:                42,
		}},
	})
	got, _ = client.GetMachine(ctx, connect.NewRequest(&pb.GetMachineRequest{MachineId: "m1"}))
	if !got.Msg.GetStrategies()[0].GetConverged() {
		t.Fatalf("expected converged after HEALTHY with matching digest")
	}

	// WatchMachine: first event is full snapshot; a later Deploy must produce an update.
	watchCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	stream, err := client.WatchMachine(watchCtx, connect.NewRequest(&pb.GetMachineRequest{MachineId: "m1"}))
	if err != nil {
		t.Fatal(err)
	}
	if !stream.Receive() {
		t.Fatalf("expected initial snapshot: %v", stream.Err())
	}
	ev := stream.Msg()
	if ev.GetMachine().GetStrategies()[0].GetPhase() != pb.DeployPhase_DEPLOY_PHASE_HEALTHY {
		t.Fatalf("watch snapshot phase unexpected")
	}

	go func() {
		time.Sleep(50 * time.Millisecond)
		_, _ = client.Deploy(ctx, connect.NewRequest(&pb.DeployRequest{
			MachineId: "m1", Strategy: "s", ArtifactVersion: "v2",
		}))
	}()
	sawV2 := false
	for stream.Receive() {
		m := stream.Msg().GetMachine()
		if m.GetStrategies()[0].GetDesiredArtifact().GetVersion() == "v2" {
			sawV2 = true
			cancel() // close the watch
			break
		}
	}
	if !sawV2 {
		t.Fatalf("did not see v2 via WatchMachine: %v", stream.Err())
	}
	_ = io.EOF
}

func TestRollbackToPrevious(t *testing.T) {
	client, st, _, _ := startHumanAPI(t)
	ctx := context.Background()
	st.UpsertMachine(&pb.Register{MachineId: "m1"})
	client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "s", Version: "v1", Digest: "sha256:aaa", Uri: "file:///a"},
	}))
	client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "s", Version: "v2", Digest: "sha256:bbb", Uri: "file:///b"},
	}))
	client.Deploy(ctx, connect.NewRequest(&pb.DeployRequest{MachineId: "m1", Strategy: "s", ArtifactVersion: "v1"}))
	client.Deploy(ctx, connect.NewRequest(&pb.DeployRequest{MachineId: "m1", Strategy: "s", ArtifactVersion: "v2"}))

	resp, err := client.Rollback(ctx, connect.NewRequest(&pb.RollbackRequest{MachineId: "m1", Strategy: "s"}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.GetGeneration() < 3 {
		t.Fatalf("generation = %d", resp.Msg.GetGeneration())
	}
	got, _ := client.GetMachine(ctx, connect.NewRequest(&pb.GetMachineRequest{MachineId: "m1"}))
	if got.Msg.GetStrategies()[0].GetDesiredArtifact().GetVersion() != "v1" {
		t.Fatalf("rollback should re-point desired to v1")
	}
}
