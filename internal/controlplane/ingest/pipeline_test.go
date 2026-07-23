package ingest

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/controlplane/store"
)

type memObjects struct {
	mu      sync.Mutex
	bucket  string
	objects map[string][]byte
}

func newMemObjects(bucket string) *memObjects {
	return &memObjects{bucket: bucket, objects: map[string][]byte{}}
}

func (m *memObjects) PresignGet(context.Context, string, string, time.Duration) (string, time.Time, error) {
	return "", time.Time{}, fmt.Errorf("unused")
}

func (m *memObjects) PutObject(_ context.Context, bucket, key string, body io.Reader, _ int64) error {
	b, err := io.ReadAll(body)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.objects[bucket+"/"+key] = b
	return nil
}

func (m *memObjects) Bucket() string { return m.bucket }

func (m *memObjects) has(key string) bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	_, ok := m.objects[m.bucket+"/"+key]
	return ok
}

func sha256Of(s string) string {
	sum := sha256.Sum256([]byte(s))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func TestIngestHappyPathWithBasicAuth(t *testing.T) {
	const body = "#!/bin/sh\necho hi\n"
	digest := sha256Of(body)
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		user, pass, ok := r.BasicAuth()
		if !ok || user != "u" || pass != "p" {
			http.Error(w, "auth", http.StatusUnauthorized)
			return
		}
		_, _ = w.Write([]byte(body))
	}))
	defer srv.Close()

	// Hostname() strips port — credentials are keyed by hostname only.
	creds := &Credentials{byHost: map[string]HostCredential{
		"127.0.0.1": {Type: CredBasic, Username: "u", Password: "p"},
	}}

	st := store.NewMemory(nil)
	objs := newMemObjects("artifacts")
	svc := New(st, objs, creds, ModeCredentialedOnly, nil)
	svc.Client = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}}, //nolint:gosec // test-only
	}

	ref := &pb.ArtifactRef{Name: "s", Version: "v1", Digest: digest, Uri: srv.URL + "/bin"}
	if err := st.RegisterArtifactRecord(&store.ArtifactRecord{Ref: ref, State: store.ArtifactStatePending}); err != nil {
		t.Fatal(err)
	}
	if err := svc.Run(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	rec, ok := st.GetArtifactRecord("s", "v1")
	if !ok || rec.State != store.ArtifactStateReady {
		t.Fatalf("record = %+v ok=%v", rec, ok)
	}
	if !strings.HasPrefix(rec.Ref.GetUri(), "s3://artifacts/artifacts/s/v1/") {
		t.Fatalf("uri = %q", rec.Ref.GetUri())
	}
	if !objs.has("artifacts/s/v1/" + strings.TrimPrefix(digest, "sha256:")) {
		t.Fatal("object missing from store")
	}
}

func TestIngestDigestMismatchLeavesNoObject(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("wrong-bytes"))
	}))
	defer srv.Close()

	st := store.NewMemory(nil)
	objs := newMemObjects("artifacts")
	svc := New(st, objs, mustEmptyCreds(t), ModeAlways, nil)
	ref := &pb.ArtifactRef{
		Name: "s", Version: "v1", Digest: sha256Of("expected"),
		Uri: srv.URL + "/bin",
	}
	_ = st.RegisterArtifactRecord(&store.ArtifactRecord{Ref: ref, State: store.ArtifactStatePending})
	err := svc.Run(context.Background(), ref)
	if err == nil {
		t.Fatal("expected digest mismatch")
	}
	rec, _ := st.GetArtifactRecord("s", "v1")
	if rec.State != store.ArtifactStateFailed || !strings.Contains(rec.StateReason, "digest mismatch") {
		t.Fatalf("state=%s reason=%q", rec.State, rec.StateReason)
	}
	if len(objs.objects) != 0 {
		t.Fatalf("store should be empty, got %d objects", len(objs.objects))
	}
}

func TestNeedsIngestCredentialedOnly(t *testing.T) {
	t.Setenv("GH_PAT", "x")
	path := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(path, []byte(`
artifact_credentials:
  api.github.com:
    type: bearer
    token_env: GH_PAT
`), 0o600)
	creds, err := LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	svc := New(store.NewMemory(nil), newMemObjects("b"), creds, ModeCredentialedOnly, nil)
	if !svc.NeedsIngest("https://api.github.com/repos/o/r/releases/assets/1") {
		t.Fatal("expected credentialed host to ingest")
	}
	if svc.NeedsIngest("https://example.com/bin") {
		t.Fatal("public host should not ingest in credentialed-only")
	}
	if svc.NeedsIngest("file:///tmp/x") {
		t.Fatal("file:// should not ingest")
	}
}

func TestValidateSourceHTTPSOnlyWithCreds(t *testing.T) {
	t.Setenv("GH_PAT", "x")
	path := filepath.Join(t.TempDir(), "c.yaml")
	_ = os.WriteFile(path, []byte(`
artifact_credentials:
  example.com:
    type: bearer
    token_env: GH_PAT
`), 0o600)
	creds, _ := LoadCredentials(path)
	svc := New(store.NewMemory(nil), newMemObjects("b"), creds, ModeCredentialedOnly, nil)
	if err := svc.ValidateSource("http://example.com/bin"); err == nil {
		t.Fatal("http + credential should be rejected")
	}
}

func TestFailInterrupted(t *testing.T) {
	st := store.NewMemory(nil)
	_ = st.RegisterArtifactRecord(&store.ArtifactRecord{
		Ref: &pb.ArtifactRef{Name: "s", Version: "v1", Digest: "sha256:a", Uri: "https://example.com/a"},
		State: store.ArtifactStatePending,
	})
	_ = st.RegisterArtifactRecord(&store.ArtifactRecord{
		Ref: &pb.ArtifactRef{Name: "s", Version: "v2", Digest: "sha256:b", Uri: "https://example.com/b"},
		State: store.ArtifactStateReady,
	})
	svc := New(st, newMemObjects("b"), mustEmptyCreds(t), ModeAlways, nil)
	svc.FailInterrupted()
	rec, _ := st.GetArtifactRecord("s", "v1")
	if rec.State != store.ArtifactStateFailed || rec.StateReason != "interrupted by restart" {
		t.Fatalf("pending = %+v", rec)
	}
	rec2, _ := st.GetArtifactRecord("s", "v2")
	if rec2.State != store.ArtifactStateReady {
		t.Fatalf("ready should stay ready: %+v", rec2)
	}
}

func mustEmptyCreds(t *testing.T) *Credentials {
	t.Helper()
	c, err := LoadCredentials("")
	if err != nil {
		t.Fatal(err)
	}
	return c
}
