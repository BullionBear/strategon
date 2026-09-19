package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/gen/strategyplatform/v1/strategyplatformv1connect"
	"github.com/bullionbear/strategon/internal/auth"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"github.com/bullionbear/strategon/internal/secrets"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func testSealKey() []byte { return bytes.Repeat([]byte{0x11}, 32) }

func startHumanAPIWithSecrets(t *testing.T) (strategyplatformv1connect.ControlPlaneServiceClient, store.Store, *secrets.Module, *stubAgents) {
	t.Helper()
	hub := store.NewHub()
	st := store.NewMemory(hub)
	agents := &stubAgents{}
	mod, err := secrets.New(secrets.Config{Key: testSealKey(), KeyID: "test"}, st)
	if err != nil {
		t.Fatal(err)
	}
	srv := New(st, hub, agents, nil).WithSecrets(mod)
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
	return client, st, mod, agents
}

func TestPutGetListSecretNoPlaintext(t *testing.T) {
	client, _, _, _ := startHumanAPIWithSecrets(t)
	ctx := context.Background()
	put, err := client.PutSecret(ctx, connect.NewRequest(&pb.PutSecretRequest{
		Name: "db-url", Value: "postgres://s3cret",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if put.Msg.GetToken() != "secret.db-url" {
		t.Fatalf("token = %q", put.Msg.GetToken())
	}
	got, err := client.GetSecret(ctx, connect.NewRequest(&pb.GetSecretRequest{Name: "db-url"}))
	if err != nil {
		t.Fatal(err)
	}
	sec := got.Msg.GetSecret()
	if sec.GetToken() != "secret.db-url" || sec.GetLengthBytes() != int32(len("postgres://s3cret")) {
		t.Fatalf("view = %+v", sec)
	}
	if strings.Contains(sec.String(), "postgres://s3cret") {
		t.Fatalf("GetSecret leaked plaintext: %s", sec)
	}
	list, err := client.ListSecrets(ctx, connect.NewRequest(&pb.ListSecretsRequest{}))
	if err != nil || len(list.Msg.GetSecrets()) != 1 {
		t.Fatalf("list: %v %#v", err, list.Msg.GetSecrets())
	}
	if strings.Contains(list.Msg.String(), "postgres://s3cret") {
		t.Fatal("ListSecrets leaked plaintext")
	}
}

func TestPutSecretAuditSafe(t *testing.T) {
	client, st, _, _ := startHumanAPIWithSecrets(t)
	ctx := context.Background()
	if _, err := client.PutSecret(ctx, connect.NewRequest(&pb.PutSecretRequest{
		Name: "db-url", Value: "postgres://s3cret",
	})); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range st.ListAudit("", "") {
		if e.GetAction() != "PutSecret" {
			continue
		}
		found = true
		if strings.Contains(e.GetDetail(), "postgres://s3cret") {
			t.Fatalf("audit leaked plaintext: %q", e.GetDetail())
		}
		if !strings.Contains(e.GetDetail(), "name=db-url") {
			t.Fatalf("detail = %q", e.GetDetail())
		}
	}
	if !found {
		t.Fatal("missing PutSecret audit")
	}
}

func TestDeleteSecret(t *testing.T) {
	client, st, _, agents := startHumanAPIWithSecrets(t)
	ctx := context.Background()
	if _, err := client.PutSecret(ctx, connect.NewRequest(&pb.PutSecretRequest{
		Name: "db-url", Value: "v1",
	})); err != nil {
		t.Fatal(err)
	}
	st.UpsertMachine(&pb.Register{MachineId: "m1"})
	if _, err := client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "hello", Version: "v1", Digest: "sha256:aaa", Uri: "file:///a"},
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ApplyAssignment(ctx, connect.NewRequest(&pb.ApplyAssignmentRequest{
		MachineId: "m1", Strategy: "hello", ArtifactVersion: "v1",
		Env: map[string]string{"DATABASE_URL": "secret.db-url"},
	})); err != nil {
		t.Fatal(err)
	}
	before := agents.n
	if _, err := client.DeleteSecret(ctx, connect.NewRequest(&pb.DeleteSecretRequest{Name: "db-url"})); err != nil {
		t.Fatal(err)
	}
	if agents.n <= before {
		t.Fatalf("DeleteSecret should Notify; n=%d before=%d", agents.n, before)
	}
	_, err := client.GetSecret(ctx, connect.NewRequest(&pb.GetSecretRequest{Name: "db-url"}))
	if err == nil || connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("get after delete: %v", err)
	}
	rec, _ := st.GetMachine("m1")
	if rec.Assignments["hello"].GetEnv()["DATABASE_URL"] != "secret.db-url" {
		t.Fatal("delete must not rewrite assignment env")
	}
	found := false
	for _, e := range st.ListAudit("", "") {
		if e.GetAction() == "DeleteSecret" && strings.Contains(e.GetDetail(), "name=db-url") {
			found = true
		}
	}
	if !found {
		t.Fatal("missing DeleteSecret audit")
	}
	_, err = client.DeleteSecret(ctx, connect.NewRequest(&pb.DeleteSecretRequest{Name: "db-url"}))
	if err == nil || connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("second delete: %v", err)
	}
}

func TestSecretRPCDark(t *testing.T) {
	client, _, _, _ := startHumanAPI(t)
	ctx := context.Background()
	_, err := client.ListSecrets(ctx, connect.NewRequest(&pb.ListSecretsRequest{}))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("list dark: %v", err)
	}
	_, err = client.ListMachines(ctx, connect.NewRequest(&pb.ListMachinesRequest{}))
	if err != nil {
		t.Fatalf("list machines while dark: %v", err)
	}
	_, err = client.DeleteSecret(ctx, connect.NewRequest(&pb.DeleteSecretRequest{Name: "db-url"}))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("delete dark: %v", err)
	}
}

func TestPutSecretNotifiesReferencingMachine(t *testing.T) {
	client, st, _, agents := startHumanAPIWithSecrets(t)
	ctx := context.Background()
	st.UpsertMachine(&pb.Register{MachineId: "m1"})
	if _, err := client.RegisterArtifact(ctx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "hello", Version: "v1", Digest: "sha256:aaa", Uri: "file:///a"},
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := client.PutSecret(ctx, connect.NewRequest(&pb.PutSecretRequest{
		Name: "db-url", Value: "v1",
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := client.ApplyAssignment(ctx, connect.NewRequest(&pb.ApplyAssignmentRequest{
		MachineId: "m1", Strategy: "hello", ArtifactVersion: "v1",
		Env: map[string]string{"DATABASE_URL": "secret.db-url"},
	})); err != nil {
		t.Fatal(err)
	}
	before := agents.n
	if _, err := client.PutSecret(ctx, connect.NewRequest(&pb.PutSecretRequest{
		Name: "db-url", Value: "v2",
	})); err != nil {
		t.Fatal(err)
	}
	if agents.n <= before {
		t.Fatalf("PutSecret should Notify referencing machine; n=%d before=%d", agents.n, before)
	}
	rec, _ := st.GetMachine("m1")
	if rec.Assignments["hello"].GetEnv()["DATABASE_URL"] != "secret.db-url" {
		t.Fatalf("stored env mutated: %#v", rec.Assignments["hello"].GetEnv())
	}
}
