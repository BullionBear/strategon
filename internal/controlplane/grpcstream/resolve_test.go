package grpcstream

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"github.com/bullionbear/strategon/internal/mtls"
)

type fakeObjects struct {
	calls []string
	url   string
	err   error
}

func (f *fakeObjects) PresignGet(_ context.Context, bucket, key string, _ time.Duration) (string, time.Time, error) {
	f.calls = append(f.calls, bucket+"/"+key)
	if f.err != nil {
		return "", time.Time{}, f.err
	}
	u := f.url
	if u == "" {
		u = "https://objects.example/presigned?" + bucket + "/" + key
	}
	return u, time.Now().Add(5 * time.Minute), nil
}

func (f *fakeObjects) PutObject(context.Context, string, string, io.Reader, int64) error {
	return nil
}

func (f *fakeObjects) Bucket() string { return "artifacts" }

func seedAssigned(t *testing.T, st *store.Memory, machineID string, art *pb.ArtifactRef) {
	t.Helper()
	if _, err := st.UpsertMachine(&pb.Register{MachineId: machineID}); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterArtifact(art); err != nil {
		t.Fatal(err)
	}
	_, err := st.SetAssignment(machineID, "strat", &pb.StrategyAssignmentSpec{
		Strategy: "strat",
		Artifact: art,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestResolveArtifactSourceAssignedOK(t *testing.T) {
	st := store.NewMemory(nil)
	art := &pb.ArtifactRef{
		Name: "s", Version: "v1", Digest: "sha256:aaa",
		Uri: "s3://artifacts/s/v1/aaa",
	}
	seedAssigned(t, st, "m1", art)
	objs := &fakeObjects{}
	srv := New(st, WithObjectStore(objs))

	ctx := mtls.ContextWithPeerCN(context.Background(), "m1")
	resp, err := srv.ResolveArtifactSource(ctx, connect.NewRequest(&pb.ResolveArtifactSourceRequest{
		Name: "s", Version: "v1",
	}))
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if !strings.Contains(resp.Msg.GetUrl(), "artifacts/s/v1/aaa") {
		t.Fatalf("url = %q, want presigned for object key", resp.Msg.GetUrl())
	}
	if resp.Msg.GetExpiresAt() == nil {
		t.Fatal("expires_at missing")
	}
	if len(objs.calls) != 1 || objs.calls[0] != "artifacts/s/v1/aaa" {
		t.Fatalf("presign calls = %v", objs.calls)
	}
}

func TestResolveArtifactSourceDeniedNotAssigned(t *testing.T) {
	st := store.NewMemory(nil)
	art := &pb.ArtifactRef{
		Name: "s", Version: "v1", Digest: "sha256:aaa",
		Uri: "s3://artifacts/s/v1/aaa",
	}
	if _, err := st.UpsertMachine(&pb.Register{MachineId: "m1"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertMachine(&pb.Register{MachineId: "m2"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterArtifact(art); err != nil {
		t.Fatal(err)
	}
	// Only m1 is assigned.
	if _, err := st.SetAssignment("m1", "strat", &pb.StrategyAssignmentSpec{
		Strategy: "strat", Artifact: art,
	}); err != nil {
		t.Fatal(err)
	}
	srv := New(st, WithObjectStore(&fakeObjects{}))

	ctx := mtls.ContextWithPeerCN(context.Background(), "m2")
	_, err := srv.ResolveArtifactSource(ctx, connect.NewRequest(&pb.ResolveArtifactSourceRequest{
		Name: "s", Version: "v1",
	}))
	if err == nil {
		t.Fatal("expected PermissionDenied")
	}
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("code = %v, want PermissionDenied", connect.CodeOf(err))
	}
	audits := st.ListAudit("m2", "s")
	if len(audits) == 0 || audits[0].GetAction() != "ResolveArtifactSource" {
		t.Fatalf("expected ResolveArtifactSource audit, got %+v", audits)
	}
	if !strings.Contains(audits[0].GetDetail(), "denied") {
		t.Fatalf("detail = %q, want denied reason", audits[0].GetDetail())
	}
}

func TestResolveArtifactSourceSharedFileOK(t *testing.T) {
	st := store.NewMemory(nil)
	art := &pb.ArtifactRef{
		Name: "instruments.json", Version: "v1", Digest: "sha256:bbb",
		Uri: "s3://artifacts/instruments.json/v1/bbb",
	}
	if _, err := st.UpsertMachine(&pb.Register{MachineId: "m1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterArtifact(art); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := st.SetSharedFiles("m1", []*pb.SharedFileSpec{
		{Name: "instruments.json", Artifact: art},
	}); err != nil {
		t.Fatal(err)
	}
	srv := New(st, WithObjectStore(&fakeObjects{}))
	ctx := mtls.ContextWithPeerCN(context.Background(), "m1")
	resp, err := srv.ResolveArtifactSource(ctx, connect.NewRequest(&pb.ResolveArtifactSourceRequest{
		Name: "instruments.json", Version: "v1",
	}))
	if err != nil {
		t.Fatalf("resolve shared: %v", err)
	}
	if resp.Msg.GetUrl() == "" {
		t.Fatal("empty url")
	}
}

func TestResolveArtifactSourceConfigRefOK(t *testing.T) {
	st := store.NewMemory(nil)
	bin := &pb.ArtifactRef{Name: "s", Version: "v1", Digest: "sha256:aaa", Uri: "file:///tmp/a"}
	cfg := &pb.ArtifactRef{Name: "s-config", Version: "c1", Digest: "sha256:ccc", Uri: "s3://artifacts/s-config/c1/ccc"}
	if _, err := st.UpsertMachine(&pb.Register{MachineId: "m1"}); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterArtifact(bin); err != nil {
		t.Fatal(err)
	}
	if err := st.RegisterArtifact(cfg); err != nil {
		t.Fatal(err)
	}
	if _, err := st.SetAssignment("m1", "strat", &pb.StrategyAssignmentSpec{
		Strategy: "strat", Artifact: bin, Config: cfg,
	}); err != nil {
		t.Fatal(err)
	}
	srv := New(st, WithObjectStore(&fakeObjects{}))
	ctx := mtls.ContextWithPeerCN(context.Background(), "m1")
	if _, err := srv.ResolveArtifactSource(ctx, connect.NewRequest(&pb.ResolveArtifactSourceRequest{
		Name: "s-config", Version: "c1",
	})); err != nil {
		t.Fatalf("resolve config: %v", err)
	}
}

func TestResolveArtifactSourceRequiresMTLS(t *testing.T) {
	st := store.NewMemory(nil)
	srv := New(st, WithObjectStore(&fakeObjects{}))
	_, err := srv.ResolveArtifactSource(context.Background(), connect.NewRequest(&pb.ResolveArtifactSourceRequest{
		Name: "s", Version: "v1",
	}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("code = %v, want Unauthenticated", connect.CodeOf(err))
	}
}

func TestResolveArtifactSourceRejectsNonS3(t *testing.T) {
	st := store.NewMemory(nil)
	art := &pb.ArtifactRef{
		Name: "s", Version: "v1", Digest: "sha256:aaa",
		Uri: "https://example.com/bin",
	}
	seedAssigned(t, st, "m1", art)
	srv := New(st, WithObjectStore(&fakeObjects{}))
	ctx := mtls.ContextWithPeerCN(context.Background(), "m1")
	_, err := srv.ResolveArtifactSource(ctx, connect.NewRequest(&pb.ResolveArtifactSourceRequest{
		Name: "s", Version: "v1",
	}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("code = %v, want FailedPrecondition", connect.CodeOf(err))
	}
}
