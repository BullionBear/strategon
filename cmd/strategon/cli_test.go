package main

import (
	"bytes"
	"context"
	"github.com/bullionbear/strategon/internal/controlplane/assignmentset"
	"gopkg.in/yaml.v3"
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

func TestApplyExampleHitsApplyAssignmentSetNotDeploy(t *testing.T) {
	client, st, agents := startCLIAPI(t)
	seedNATSExample(t, client, st)
	ctx := context.Background()

	n := agents.n
	results, err := applyFile(ctx, client, exampleClusterYAML(t))
	if err != nil {
		t.Fatal(err)
	}
	if len(results) != 1 || results[0].RPC != "ApplyAssignmentSet" || results[0].Name != "trading" {
		t.Fatalf("results = %+v", results)
	}
	if results[0].Generation != 1 {
		t.Fatalf("generation = %d", results[0].Generation)
	}
	if agents.n != n {
		t.Fatalf("ApplyAssignmentSet must not notify agents (got %d, want %d)", agents.n, n)
	}
	for _, id := range []string{"m1", "m2", "m3"} {
		rec, _ := st.GetMachine(id)
		if rec.Assignments["nats"] != nil {
			t.Fatalf("member %s got a nats assignment from apply", id)
		}
	}
	c, ok := st.GetAssignmentSet("trading")
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

func TestApplyMachineVolumesEnsureOnly(t *testing.T) {
	client, st, _ := startCLIAPI(t)
	st.UpsertMachine(&pb.Register{MachineId: "m1"})
	ctx := context.Background()
	yaml := `
kind: MachineVolumes
metadata:
  name: m1
spec:
  volumes:
    - name: mftik-data
    - name: nats-a-data
`
	results, err := applyReader(ctx, client, strings.NewReader(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if results[0].Kind != "MachineVolumes" || results[0].RPC != "CreateVolume" {
		t.Fatalf("results = %+v", results)
	}
	rec, _ := st.GetMachine("m1")
	if rec.Volumes["mftik-data"] == nil || rec.Volumes["nats-a-data"] == nil {
		t.Fatalf("volumes = %+v", rec.Volumes)
	}
	again, err := applyReader(ctx, client, strings.NewReader(yaml))
	if err != nil {
		t.Fatal(err)
	}
	if again[0].Generation != results[0].Generation {
		t.Fatalf("ensure-only re-apply bumped generation")
	}
	if _, err := client.CreateVolume(ctx, connect.NewRequest(&pb.CreateVolumeRequest{
		MachineId: "m1", Name: "extra",
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := applyReader(ctx, client, strings.NewReader(yaml)); err != nil {
		t.Fatal(err)
	}
	rec, _ = st.GetMachine("m1")
	if rec.Volumes["extra"] == nil {
		t.Fatal("ensure-only apply must not delete extra volumes")
	}
}

func TestVolumeCLICreateListDelete(t *testing.T) {
	client, st, _ := startCLIAPI(t)
	st.UpsertMachine(&pb.Register{MachineId: "m1"})
	ctx := context.Background()
	if _, err := client.CreateVolume(ctx, connect.NewRequest(&pb.CreateVolumeRequest{
		MachineId: "m1", Name: "data",
	})); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := listVolumes(ctx, client, "m1", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "data") {
		t.Fatalf("list = %q", buf.String())
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

func TestApplyCaptureStdioYAML(t *testing.T) {
	client, st, _ := startCLIAPI(t)
	ctx := context.Background()
	st.UpsertMachine(&pb.Register{MachineId: "m1", AgentVersion: 4})
	client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "hello", Version: "v1", Digest: "sha256:h", Uri: "file:///hello"},
	}))
	p := filepath.Join(t.TempDir(), "cap.yaml")
	body := []byte(`apiVersion: strategon/v1
kind: StrategyAssignment
metadata:
  name: hello
spec:
  machineId: m1
  artifactVersion: v1
  captureStdio: true
`)
	if err := os.WriteFile(p, body, 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := applyFile(ctx, client, p); err != nil {
		t.Fatal(err)
	}
	rec, _ := st.GetMachine("m1")
	if !rec.Assignments["hello"].GetCaptureStdio() {
		t.Fatal("captureStdio not applied")
	}
}

func TestGetAndWaitAssignmentSet(t *testing.T) {
	client, st, _ := startCLIAPI(t)
	seedNATSExample(t, client, st)
	ctx := context.Background()
	if _, err := applyFile(ctx, client, exampleClusterYAML(t)); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := getAssignmentSet(ctx, client, "trading", &buf); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), "AssignmentSet trading") || !strings.Contains(buf.String(), "m1") {
		t.Fatalf("get output = %s", buf.String())
	}

	if err := st.UpdateAssignmentSetStatus("trading", &pb.AssignmentSetStatus{
		Phase:              "Ready",
		ObservedGeneration: 1,
		Members: []*pb.MemberStatus{
			{Machine: "m1", Name: "nats-m1", Ready: true, Converged: true, Phase: "HEALTHY"},
			{Machine: "m2", Name: "nats-m2", Ready: true, Converged: true, Phase: "HEALTHY"},
			{Machine: "m3", Name: "nats-m3", Ready: true, Converged: true, Phase: "HEALTHY"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	old := waitPoll
	waitPoll = 10 * time.Millisecond
	t.Cleanup(func() { waitPoll = old })
	if err := waitAssignmentSet(ctx, client, "trading", "ready", 2*time.Second); err != nil {
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

	err := waitAssignmentSet(ctx, client, "trading", "ready", 40*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "timed out") {
		t.Fatalf("want timeout, got %v", err)
	}

	if err := st.UpdateAssignmentSetStatus("trading", &pb.AssignmentSetStatus{Phase: "Failed", Message: "rolled back"}); err != nil {
		t.Fatal(err)
	}
	err = waitAssignmentSet(ctx, client, "trading", "ready", time.Second)
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
	if err := st.UpdateAssignmentSetStatus("trading", &pb.AssignmentSetStatus{
		Phase:              "Ready",
		ObservedGeneration: 0,
	}); err != nil {
		t.Fatal(err)
	}

	old := waitPoll
	waitPoll = 10 * time.Millisecond
	t.Cleanup(func() { waitPoll = old })
	err := waitAssignmentSet(ctx, client, "trading", "ready", 40*time.Millisecond)
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
	if err := st.UpdateAssignmentSetStatus("trading", &pb.AssignmentSetStatus{
		Phase:   "Degraded",
		Message: "waitReadySeconds elapsed",
	}); err != nil {
		t.Fatal(err)
	}

	old := waitPoll
	waitPoll = 10 * time.Millisecond
	t.Cleanup(func() { waitPoll = old })
	err := waitAssignmentSet(ctx, client, "trading", "ready", time.Second)
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

func TestFilesAndLogsUsage(t *testing.T) {
	for _, args := range [][]string{
		{"files"},
		{"files", "nope"},
		{"files", "ls"},
		{"files", "get"},
		{"logs"},
	} {
		err := run(args)
		if err == nil || !isUsage(err) {
			t.Fatalf("%v: %v", args, err)
		}
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

// The shipped example must survive the real YAML decoder and the control
// plane's template validation. It is the only place NATS knowledge exists.
func TestApplyExampleNatsManifest(t *testing.T) {
	data, err := os.ReadFile("../../examples/nats/cluster.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc yamlDoc
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&doc); err != nil {
		t.Fatalf("decode example: %v", err)
	}
	if !strings.EqualFold(doc.Kind, "AssignmentSet") {
		t.Fatalf("kind = %q", doc.Kind)
	}
	var spec setSpecYAML
	if err := doc.Spec.Decode(&spec); err != nil {
		t.Fatalf("decode spec: %v", err)
	}
	if spec.Strategy != "nats" || len(spec.Members) != 3 {
		t.Fatalf("strategy=%q members=%d", spec.Strategy, len(spec.Members))
	}
	if len(spec.Template.Args) == 0 || spec.Template.Peers.Format == "" {
		t.Fatalf("template did not decode: %+v", spec.Template)
	}
	// Render it the way the control plane will.
	set := &pb.AssignmentSet{
		Metadata: &pb.ObjectMeta{Name: doc.Metadata.Name},
		Spec:     &pb.AssignmentSetSpec{Strategy: spec.Strategy},
	}
	set.Spec.Template = &pb.MemberTemplate{
		Args:      spec.Template.Args,
		Env:       spec.Template.Env,
		Readiness: &pb.ReadinessProbe{Endpoint: spec.Template.Readiness.Endpoint},
		Peers: &pb.PeerList{
			Format:    spec.Template.Peers.Format,
			Separator: spec.Template.Peers.Separator,
		},
	}
	for _, m := range spec.Members {
		set.Spec.Members = append(set.Spec.Members, &pb.SetMember{Machine: m.Machine, Name: m.Name, Vars: m.Vars})
	}
	if err := assignmentset.Validate(set); err != nil {
		t.Fatalf("example manifest does not render: %v", err)
	}
	got, err := assignmentset.Expand(set, 0)
	if err != nil {
		t.Fatal(err)
	}
	want := "nats://10.0.0.2:6222,nats://10.0.0.3:6222"
	if got.Args[3] != want {
		t.Fatalf("routes = %q, want %q", got.Args[3], want)
	}
	if got.Env["NATS_CLUSTER_NAME"] != "trading" {
		t.Fatalf("cluster name = %q", got.Env["NATS_CLUSTER_NAME"])
	}
	if got.Endpoint != "http://127.0.0.1:8222/healthz" {
		t.Fatalf("endpoint = %q", got.Endpoint)
	}
}

func TestApplyExampleNatsSingleHostManifest(t *testing.T) {
	data, err := os.ReadFile("../../examples/nats/single-host.yaml")
	if err != nil {
		t.Fatal(err)
	}
	var doc yamlDoc
	if err := yaml.NewDecoder(bytes.NewReader(data)).Decode(&doc); err != nil {
		t.Fatalf("decode: %v", err)
	}
	var spec setSpecYAML
	if err := doc.Spec.Decode(&spec); err != nil {
		t.Fatal(err)
	}
	if len(spec.Members) != 3 {
		t.Fatalf("members=%d", len(spec.Members))
	}
	host := spec.Members[0].Machine
	seen := map[string]struct{}{}
	for _, m := range spec.Members {
		if m.Machine != host {
			t.Fatalf("expected one machine, got %q and %q", host, m.Machine)
		}
		if _, dup := seen[m.Name]; dup {
			t.Fatalf("duplicate member name %q", m.Name)
		}
		seen[m.Name] = struct{}{}
	}
	set := &pb.AssignmentSet{
		Metadata: &pb.ObjectMeta{Name: doc.Metadata.Name},
		Spec: &pb.AssignmentSetSpec{
			Strategy: spec.Strategy,
			Template: &pb.MemberTemplate{
				Args:      spec.Template.Args,
				Env:       spec.Template.Env,
				Readiness: &pb.ReadinessProbe{Endpoint: spec.Template.Readiness.Endpoint},
				Peers:     &pb.PeerList{Format: spec.Template.Peers.Format, Separator: spec.Template.Peers.Separator},
			},
		},
	}
	for _, m := range spec.Members {
		set.Spec.Members = append(set.Spec.Members, &pb.SetMember{Machine: m.Machine, Name: m.Name, Vars: m.Vars})
	}
	if err := assignmentset.Validate(set); err != nil {
		t.Fatalf("single-host manifest does not render: %v", err)
	}
}
