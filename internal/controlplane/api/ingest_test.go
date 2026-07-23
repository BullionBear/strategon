package api

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/controlplane/ingest"
	"github.com/bullionbear/strategon/internal/controlplane/store"
)

type memObjects struct {
	bucket  string
	objects map[string][]byte
}

func (m *memObjects) PresignGet(context.Context, string, string, time.Duration) (string, time.Time, error) {
	return "", time.Time{}, nil
}

func (m *memObjects) PutObject(_ context.Context, bucket, key string, body io.Reader, _ int64) error {
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	if m.objects == nil {
		m.objects = map[string][]byte{}
	}
	m.objects[bucket+"/"+key] = b
	return nil
}

func (m *memObjects) Bucket() string { return m.bucket }

func TestDeployBlockedWhilePending(t *testing.T) {
	st := store.NewMemory(nil)
	_, _ = st.UpsertMachine(&pb.Register{MachineId: "m1"})
	_ = st.RegisterArtifactRecord(&store.ArtifactRecord{
		Ref: &pb.ArtifactRef{
			Name: "s", Version: "v1", Digest: "sha256:aaa",
			Uri: "https://example.com/bin",
		},
		State: store.ArtifactStatePending,
	})
	srv := New(st, nil, nil, nil)

	_, err := srv.Deploy(context.Background(), connect.NewRequest(&pb.DeployRequest{
		MachineId: "m1", Strategy: "s", ArtifactVersion: "v1",
	}))
	if err == nil || connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("err=%v, want FailedPrecondition", err)
	}
	if !strings.Contains(err.Error(), "PENDING") {
		t.Fatalf("error = %v, want PENDING mention", err)
	}
}

func TestListArtifactsIncludesState(t *testing.T) {
	st := store.NewMemory(nil)
	_ = st.RegisterArtifactRecord(&store.ArtifactRecord{
		Ref: &pb.ArtifactRef{
			Name: "s", Version: "v1", Digest: "sha256:aaa", Uri: "file:///tmp/a",
		},
		State:       store.ArtifactStateFailed,
		StateReason: "digest mismatch",
	})
	srv := New(st, nil, nil, nil)
	resp, err := srv.ListArtifacts(context.Background(), connect.NewRequest(&pb.ListArtifactsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Msg.GetEntries()) != 1 {
		t.Fatalf("entries = %d", len(resp.Msg.GetEntries()))
	}
	e := resp.Msg.GetEntries()[0]
	if e.GetState() != store.ArtifactStateFailed || e.GetStateReason() != "digest mismatch" {
		t.Fatalf("entry = %+v", e)
	}
	if len(resp.Msg.GetArtifacts()) != 1 {
		t.Fatal("artifacts field should still be populated")
	}
}

func TestRegisterArtifactIngestEndToEnd(t *testing.T) {
	const body = "payload-bytes"
	sum := sha256.Sum256([]byte(body))
	digest := "sha256:" + hex.EncodeToString(sum[:])

	hs := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(body))
	}))
	defer hs.Close()

	st := store.NewMemory(nil)
	objs := &memObjects{bucket: "artifacts"}
	creds, _ := ingest.LoadCredentials("")
	svc := ingest.New(st, objs, creds, ingest.ModeAlways, nil)
	srv := New(st, nil, nil, nil).WithIngest(svc)

	_, err := srv.RegisterArtifact(context.Background(), connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{
			Name: "s", Version: "v1", Digest: digest, Uri: hs.URL + "/bin",
		},
	}))
	if err != nil {
		t.Fatal(err)
	}
	// Wait for background ingest.
	deadline := time.Now().Add(3 * time.Second)
	for {
		rec, ok := st.GetArtifactRecord("s", "v1")
		if ok && rec.State == store.ArtifactStateReady {
			if !strings.HasPrefix(rec.Ref.GetUri(), "s3://artifacts/") {
				t.Fatalf("uri = %q", rec.Ref.GetUri())
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for READY; got %+v", rec)
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Deploy should now succeed past the READY guard (machine must exist).
	_, _ = st.UpsertMachine(&pb.Register{MachineId: "m1"})
	_, err = srv.Deploy(context.Background(), connect.NewRequest(&pb.DeployRequest{
		MachineId: "m1", Strategy: "s", ArtifactVersion: "v1",
	}))
	if err != nil {
		t.Fatalf("deploy after READY: %v", err)
	}
}
