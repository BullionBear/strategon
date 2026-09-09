package main

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/gen/strategyplatform/v1/strategyplatformv1connect"
	"github.com/bullionbear/strategon/internal/auth"
	"github.com/bullionbear/strategon/internal/controlplane/api"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

type stubAgents struct{ n int }

func (s *stubAgents) Notify(string) { s.n++ }

func startCLIAPI(t *testing.T) (strategyplatformv1connect.ControlPlaneServiceClient, store.Store, *stubAgents) {
	t.Helper()
	hub := store.NewHub()
	st := store.NewMemory(hub)
	agents := &stubAgents{}
	srv := api.New(st, hub, agents, nil)
	authSvc, err := auth.New(auth.Config{Mode: auth.ModeNone, SessionSecret: "test-secret-at-least-32-bytes-long!!"})
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	path, h := strategyplatformv1connect.NewControlPlaneServiceHandler(srv, authSvc.HandlerOptions()...)
	mux.Handle(path, h)
	ts := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	ts.Start()
	t.Cleanup(ts.Close)
	client := strategyplatformv1connect.NewControlPlaneServiceClient(http.DefaultClient, ts.URL)
	return client, st, agents
}

func seedNATSExample(t *testing.T, client strategyplatformv1connect.ControlPlaneServiceClient, st store.Store) {
	t.Helper()
	ctx := context.Background()
	for _, id := range []string{"m1", "m2", "m3"} {
		if _, err := st.UpsertMachine(&pb.Register{MachineId: id, Hostname: id}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "nats", Version: "v2.10.24", Digest: "sha256:nats", Uri: "file:///nats"},
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "nats-config", Version: "v1", Digest: "sha256:cfg", Uri: "file:///nats.conf"},
	})); err != nil {
		t.Fatal(err)
	}
}

func exampleClusterYAML(t *testing.T) string {
	t.Helper()
	p := filepath.Join("..", "..", "examples", "nats", "cluster.yaml")
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("example cluster.yaml: %v", err)
	}
	return p
}

func TestApplyExampleHitsApplyNatsClusterNotDeploy(t *testing.T) {
	client, st, agents := startCLIAPI(t)
	seedNATSExample(t, client, st)
	ctx := context.Background()

	n := agents.n
	results, err := applyFile(ctx, client, exampleClusterYAML(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].RPC != "ApplyNatsCluster" || results[0].Name != "trading" {
		t.Fatalf("results = %+v", results)
	}
	if results[0].Generation != 1 {
		t.Fatalf("generation = %d", results[0].Generation)
	}
	if agents.n != n {
		t.Fatalf("ApplyNatsCluster must not notify agents (got %d, want %d)", agents.n, n)
	}
	for _, id := range []string{"m1", "m2", "m3"} {
		rec, _ := st.GetMachine(id)
		if rec.Assignments["nats"] != nil {
			t.Fatalf("member %s got a nats assignment from apply", id)
		}
	}
	c, ok := st.GetNatsCluster("trading")
	if !ok {
		t.Fatal("cluster not persisted")
	}

	again, err := applyFile(ctx, client, exampleClusterYAML(t))
	if err != nil {
		t.Fatal(err)
	}
	if again[0].Generation != c.GetMetadata().GetGeneration() {
		t.Fatalf("re-apply bumped generation %d → %d", c.GetMetadata().GetGeneration(), again[0].Generation)
	}
	if agents.n != n {
		t.Fatal("re-apply notified agents")
	}
}

func TestApplyUnknownKind(t *testing.T) {
	client, _, _ := startCLIAPI(t)
	_, err := applyReader(context.Background(), client, strings.NewReader("kind: Widget\nmetadata:\n  name: x\n"))
	if err == nil || !strings.Contains(err.Error(), "unknown kind") {
		t.Fatalf("want unknown kind error, got %v", err)
	}
}

func TestApplyStrategyAssignment(t *testing.T) {
	client, st, _ := startCLIAPI(t)
	ctx := context.Background()
	st.UpsertMachine(&pb.Register{MachineId: "m1"})
	client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "hello", Version: "v1", Digest: "sha256:h", Uri: "file:///hello"},
	}))
	p := filepath.Join("..", "..", "examples", "assignment", "hello.yaml")
	results, err := applyFile(ctx, client, p)
	if err != nil {
		t.Fatal(err)
	}
	if results[0].RPC != "ApplyAssignment" || results[0].Name != "hello" {
		t.Fatalf("results = %+v", results)
	}
	rec, _ := st.GetMachine("m1")
	if rec.Assignments["hello"] == nil || rec.Assignments["hello"].GetStopped() {
		t.Fatalf("assignment = %+v", rec.Assignments["hello"])
	}
}

func TestGetAndWaitNatsCluster(t *testing.T) {
	client, st, _ := startCLIAPI(t)
	seedNATSExample(t, client, st)
	ctx := context.Background()
	if _, err := applyFile(ctx, client, exampleClusterYAML(t)); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := getNatsCluster(ctx, client, "trading", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "NatsCluster trading") || !strings.Contains(buf.String(), "m1") {
		t.Fatalf("get output = %s", buf.String())
	}

	if err := st.UpdateNatsClusterStatus("trading", &pb.NatsClusterStatus{
		Phase:              "Ready",
		ObservedGeneration: 1,
		Servers: []*pb.NatsServerStatus{
			{Machine: "m1", ServerName: "nats-m1", Ready: true, Converged: true, Phase: "HEALTHY"},
			{Machine: "m2", ServerName: "nats-m2", Ready: true, Converged: true, Phase: "HEALTHY"},
			{Machine: "m3", ServerName: "nats-m3", Ready: true, Converged: true, Phase: "HEALTHY"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	old := waitPoll
	waitPoll = 10 * time.Millisecond
	t.Cleanup(func() { waitPoll = old })
	if err := waitNatsCluster(ctx, client, "trading", "ready", 2*time.Second); err != nil {
		t.Fatal(err)
	}
}

func TestWaitTimeoutAndFailed(t *testing.T) {
	client, st, _ := startCLIAPI(t)
	seedNATSExample(t, client, st)
	ctx := context.Background()
	if _, err := applyFile(ctx, client, exampleClusterYAML(t)); err != nil {
		t.Fatal(err)
	}

	old := waitPoll
	waitPoll = 10 * time.Millisecond
	t.Cleanup(func() { waitPoll = old })

	err := waitNatsCluster(ctx, client, "trading", "ready", 40*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want timeout, got %v", err)
	}

	if err := st.UpdateNatsClusterStatus("trading", &pb.NatsClusterStatus{Phase: "Failed", Message: "rolled back"}); err != nil {
		t.Fatal(err)
	}
	err = waitNatsCluster(ctx, client, "trading", "ready", time.Second)
	if err == nil || !strings.Contains(err.Error(), "Failed") {
		t.Fatalf("want Failed, got %v", err)
	}
}

func TestWaitStaleReadyDoesNotSucceed(t *testing.T) {
	client, st, _ := startCLIAPI(t)
	seedNATSExample(t, client, st)
	ctx := context.Background()
	if _, err := applyFile(ctx, client, exampleClusterYAML(t)); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNatsClusterStatus("trading", &pb.NatsClusterStatus{
		Phase:              "Ready",
		ObservedGeneration: 0,
	}); err != nil {
		t.Fatal(err)
	}

	old := waitPoll
	waitPoll = 10 * time.Millisecond
	t.Cleanup(func() { waitPoll = old })
	err := waitNatsCluster(ctx, client, "trading", "ready", 40*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("stale Ready must time out, got %v", err)
	}
}

func TestWaitDegradedFailsFast(t *testing.T) {
	client, st, _ := startCLIAPI(t)
	seedNATSExample(t, client, st)
	ctx := context.Background()
	if _, err := applyFile(ctx, client, exampleClusterYAML(t)); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNatsClusterStatus("trading", &pb.NatsClusterStatus{
		Phase:   "Degraded",
		Message: "waitReadySeconds elapsed",
	}); err != nil {
		t.Fatal(err)
	}

	old := waitPoll
	waitPoll = 10 * time.Millisecond
	t.Cleanup(func() { waitPoll = old })
	err := waitNatsCluster(ctx, client, "trading", "ready", time.Second)
	if err == nil || !strings.Contains(err.Error(), "Degraded") {
		t.Fatalf("want Degraded, got %v", err)
	}
}

func TestPeelLeadingGlobals(t *testing.T) {
	t.Setenv("STRATEGON_ADDR", "")
	t.Setenv("STRATEGON_TOKEN", "")
	rest := peelLeadingGlobals([]string{"--addr", "http://example:8081", "--token", "t", "apply", "-f", "x.yaml"})
	if strings.Join(rest, " ") != "apply -f x.yaml" {
		t.Fatalf("rest = %v", rest)
	}
	if os.Getenv("STRATEGON_ADDR") != "http://example:8081" || os.Getenv("STRATEGON_TOKEN") != "t" {
		t.Fatalf("env addr=%s token=%s", os.Getenv("STRATEGON_ADDR"), os.Getenv("STRATEGON_TOKEN"))
	}
}

func TestRunUnknownCommandAndKind(t *testing.T) {
	if err := run([]string{"explode"}); err == nil || !isUsage(err) {
		t.Fatalf("unknown command: %v", err)
	}
	err := run([]string{"apply", "-f", filepath.Join("testdata", "does-not-exist.yaml")})
	if err == nil {
		t.Fatal("missing file should fail")
	}
}
