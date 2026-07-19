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

func TestGetControlPlaneVersion(t *testing.T) {
	client, _, _, _ := startHumanAPI(t)
	resp, err := client.GetControlPlaneVersion(context.Background(), connect.NewRequest(&pb.GetControlPlaneVersionRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.GetVersion() == "" {
		t.Fatal("expected non-empty version (default \"dev\" for bare builds)")
	}
}

func TestMachineSurfacesBuildVersion(t *testing.T) {
	client, st, _, _ := startHumanAPI(t)
	if _, err := st.UpsertMachine(&pb.Register{
		MachineId: "m1", Hostname: "host1", AgentVersion: 1, AgentBuildVersion: "v1.4.2-3-gabc1234",
	}); err != nil {
		t.Fatal(err)
	}
	got, err := client.GetMachine(context.Background(), connect.NewRequest(&pb.GetMachineRequest{MachineId: "m1"}))
	if err != nil {
		t.Fatal(err)
	}
	if got.Msg.GetAgentBuildVersion() != "v1.4.2-3-gabc1234" || got.Msg.GetAgentVersion() != 1 {
		t.Fatalf("unexpected machine versions: %+v", got.Msg)
	}
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

func TestUndeployRemovesAssignment(t *testing.T) {
	client, st, _, agents := startHumanAPI(t)
	ctx := context.Background()
	st.UpsertMachine(&pb.Register{MachineId: "m1"})
	client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "s", Version: "v1", Digest: "sha256:aaa", Uri: "file:///a"},
	}))
	deployResp, err := client.Deploy(ctx, connect.NewRequest(&pb.DeployRequest{
		MachineId: "m1", Strategy: "s", ArtifactVersion: "v1",
	}))
	if err != nil {
		t.Fatal(err)
	}
	notifyAfterDeploy := agents.n

	resp, err := client.Undeploy(ctx, connect.NewRequest(&pb.UndeployRequest{
		MachineId: "m1", Strategy: "s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.GetGeneration() <= deployResp.Msg.GetGeneration() {
		t.Fatalf("generation = %d, want > %d", resp.Msg.GetGeneration(), deployResp.Msg.GetGeneration())
	}
	if agents.n != notifyAfterDeploy+1 {
		t.Fatalf("agent notify count = %d, want %d", agents.n, notifyAfterDeploy+1)
	}

	rec, ok := st.GetMachine("m1")
	if !ok {
		t.Fatal("machine missing")
	}
	if _, assigned := rec.Assignments["s"]; assigned {
		t.Fatal("assignment should be removed")
	}
	got, err := client.GetMachine(ctx, connect.NewRequest(&pb.GetMachineRequest{MachineId: "m1"}))
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Msg.GetStrategies()) != 0 {
		t.Fatalf("strategies = %d, want 0", len(got.Msg.GetStrategies()))
	}

	audits := st.ListAudit("m1", "s")
	found := false
	for _, a := range audits {
		if a.GetAction() == "Undeploy" && a.GetFromVersion() == "v1" {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("expected Undeploy audit entry, got %+v", audits)
	}
}

func TestSetDeploymentSetsArgsEnvAndConfig(t *testing.T) {
	client, st, _, agents := startHumanAPI(t)
	ctx := context.Background()
	st.UpsertMachine(&pb.Register{MachineId: "m1"})
	client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "s", Version: "v1", Digest: "sha256:aaa", Uri: "file:///a"},
	}))
	client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "s-config", Version: "c17", Digest: "sha256:cfg", Uri: "file:///c.yml"},
	}))

	resp, err := client.SetDeployment(ctx, connect.NewRequest(&pb.SetDeploymentRequest{
		MachineId:       "m1",
		Strategy:        "s",
		ArtifactVersion: "v1",
		ConfigVersion:   "c17",
		Args:            []string{"-c", "${CONFIG}"},
		Env:             map[string]string{"FOO": "bar"},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.GetGeneration() != 1 {
		t.Fatalf("generation = %d, want 1", resp.Msg.GetGeneration())
	}
	if agents.n != 1 {
		t.Fatalf("agent notify count = %d, want 1", agents.n)
	}

	rec, ok := st.GetMachine("m1")
	if !ok {
		t.Fatal("machine missing")
	}
	spec := rec.Assignments["s"]
	if spec == nil {
		t.Fatal("assignment missing")
	}
	if spec.GetArtifact().GetVersion() != "v1" {
		t.Fatalf("artifact = %q", spec.GetArtifact().GetVersion())
	}
	if spec.GetConfig().GetVersion() != "c17" {
		t.Fatalf("config = %q", spec.GetConfig().GetVersion())
	}
	if len(spec.GetArgs()) != 2 || spec.GetArgs()[0] != "-c" || spec.GetArgs()[1] != "${CONFIG}" {
		t.Fatalf("args = %#v", spec.GetArgs())
	}
	if spec.GetEnv()["FOO"] != "bar" {
		t.Fatalf("env = %#v", spec.GetEnv())
	}

	// Deploy (version-only) must preserve args/env.
	client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "s", Version: "v2", Digest: "sha256:bbb", Uri: "file:///b"},
	}))
	_, err = client.Deploy(ctx, connect.NewRequest(&pb.DeployRequest{
		MachineId: "m1", Strategy: "s", ArtifactVersion: "v2",
	}))
	if err != nil {
		t.Fatal(err)
	}
	rec, _ = st.GetMachine("m1")
	spec = rec.Assignments["s"]
	if spec.GetArtifact().GetVersion() != "v2" {
		t.Fatalf("after Deploy artifact = %q", spec.GetArtifact().GetVersion())
	}
	if len(spec.GetArgs()) != 2 || spec.GetArgs()[1] != "${CONFIG}" || spec.GetEnv()["FOO"] != "bar" {
		t.Fatalf("Deploy should preserve args/env, got args=%#v env=%#v", spec.GetArgs(), spec.GetEnv())
	}
	if spec.GetConfig().GetVersion() != "c17" {
		t.Fatalf("Deploy should keep config, got %q", spec.GetConfig().GetVersion())
	}
}

func TestSetDeploymentResolvesLatestToConcreteVersion(t *testing.T) {
	client, st, _, _ := startHumanAPI(t)
	ctx := context.Background()
	st.UpsertMachine(&pb.Register{MachineId: "m1"})
	client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "s", Version: "v1", Digest: "sha256:aaa", Uri: "file:///a"},
	}))
	client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "s", Version: "v2", Digest: "sha256:bbb", Uri: "file:///b"},
	}))
	client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "s-config", Version: "c1", Digest: "sha256:c1", Uri: "file:///c1"},
	}))
	client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "s-config", Version: "c2", Digest: "sha256:c2", Uri: "file:///c2"},
	}))

	_, err := client.SetDeployment(ctx, connect.NewRequest(&pb.SetDeploymentRequest{
		MachineId:       "m1",
		Strategy:        "s",
		ArtifactVersion: "latest",
		ConfigVersion:   "latest",
	}))
	if err != nil {
		t.Fatal(err)
	}
	rec, _ := st.GetMachine("m1")
	spec := rec.Assignments["s"]
	if spec.GetArtifact().GetVersion() != "v2" {
		t.Fatalf("artifact pinned = %q, want v2 (not latest)", spec.GetArtifact().GetVersion())
	}
	if spec.GetConfig().GetVersion() != "c2" {
		t.Fatalf("config pinned = %q, want c2 (not latest)", spec.GetConfig().GetVersion())
	}

	// Registering a newer version must not move the existing deployment.
	client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "s", Version: "v3", Digest: "sha256:ccc", Uri: "file:///c"},
	}))
	rec, _ = st.GetMachine("m1")
	if rec.Assignments["s"].GetArtifact().GetVersion() != "v2" {
		t.Fatalf("deployment must stay pinned at v2 after v3 register; got %q",
			rec.Assignments["s"].GetArtifact().GetVersion())
	}
}

func TestUndeployUnassignedFails(t *testing.T) {
	client, st, _, _ := startHumanAPI(t)
	ctx := context.Background()
	st.UpsertMachine(&pb.Register{MachineId: "m1"})

	_, err := client.Undeploy(ctx, connect.NewRequest(&pb.UndeployRequest{
		MachineId: "m1", Strategy: "s",
	}))
	if err == nil {
		t.Fatal("expected undeploy of unassigned strategy to fail")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code=%v, want FailedPrecondition", connect.CodeOf(err))
	}
}

func TestDeployBlockedWhileOtherMachineHoldsLease(t *testing.T) {
	client, st, _, _ := startHumanAPI(t)
	ctx := context.Background()
	st.UpsertMachine(&pb.Register{MachineId: "m1"})
	st.UpsertMachine(&pb.Register{MachineId: "m2"})
	client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "s", Version: "v1", Digest: "sha256:aaa", Uri: "file:///a"},
	}))

	if _, err := st.AcquireLease("m1", "s", time.Minute); err != nil {
		t.Fatal(err)
	}

	_, err := client.Deploy(ctx, connect.NewRequest(&pb.DeployRequest{
		MachineId: "m2", Strategy: "s", ArtifactVersion: "v1",
	}))
	if err == nil {
		t.Fatal("expected deploy to m2 blocked by m1 lease")
	}
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code=%v, want FailedPrecondition", connect.CodeOf(err))
	}

	// Same-machine deploy still allowed.
	if _, err := client.Deploy(ctx, connect.NewRequest(&pb.DeployRequest{
		MachineId: "m1", Strategy: "s", ArtifactVersion: "v1",
	})); err != nil {
		t.Fatalf("same-machine deploy: %v", err)
	}

	got, err := client.GetMachine(ctx, connect.NewRequest(&pb.GetMachineRequest{MachineId: "m1"}))
	if err != nil {
		t.Fatal(err)
	}
	if !got.Msg.GetStrategies()[0].GetLeaseHeld() {
		t.Fatal("expected lease_held on m1 StrategyView")
	}
}
