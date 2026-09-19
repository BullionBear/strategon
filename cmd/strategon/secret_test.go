package main

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
	"github.com/bullionbear/strategon/internal/controlplane/api"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"github.com/bullionbear/strategon/internal/secrets"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func startCLIAPIWithSecrets(t *testing.T) strategyplatformv1connect.ControlPlaneServiceClient {
	t.Helper()
	hub := store.NewHub()
	st := store.NewMemory(hub)
	mod, err := secrets.New(secrets.Config{Key: bytes.Repeat([]byte{0x11}, 32), KeyID: "test"}, st)
	if err != nil {
		t.Fatal(err)
	}
	srv := api.New(st, hub, &stubAgents{}, nil).WithSecrets(mod)
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
	return strategyplatformv1connect.NewControlPlaneServiceClient(http.DefaultClient, ts.URL)
}

func TestSecretCLIList(t *testing.T) {
	client := startCLIAPIWithSecrets(t)
	ctx := context.Background()
	if _, err := client.PutSecret(ctx, connect.NewRequest(&pb.PutSecretRequest{
		Name: "db-url", Value: "postgres://s3cret",
	})); err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := listSecrets(ctx, client, &buf); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	if !strings.Contains(out, "secret.db-url") || !strings.Contains(out, "db-url") {
		t.Fatalf("list = %q", out)
	}
	if strings.Contains(out, "postgres://s3cret") {
		t.Fatalf("list leaked plaintext: %q", out)
	}
}
