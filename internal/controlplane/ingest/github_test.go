package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/controlplane/store"
)

func TestParseGitHubReleaseURL(t *testing.T) {
	okCases := []struct {
		in   string
		want GitHubReleaseRef
	}{
		{
			"https://github.com/org/repo/releases/download/v1.0.0/strat",
			GitHubReleaseRef{Owner: "org", Repo: "repo", Tag: "v1.0.0", Asset: "strat"},
		},
		{
			"https://www.github.com/o/r/releases/download/tag/bin.tar.gz",
			GitHubReleaseRef{Owner: "o", Repo: "r", Tag: "tag", Asset: "bin.tar.gz"},
		},
		{
			"https://github.com/o/r/releases/download/v1/path/like/asset",
			GitHubReleaseRef{Owner: "o", Repo: "r", Tag: "v1", Asset: "path/like/asset"},
		},
		{
			"https://github.com/o/r/releases/download/v1/my%20bin",
			GitHubReleaseRef{Owner: "o", Repo: "r", Tag: "v1", Asset: "my bin"},
		},
	}
	for _, tc := range okCases {
		got, ok := ParseGitHubReleaseURL(tc.in)
		if !ok {
			t.Fatalf("ParseGitHubReleaseURL(%q) ok=false", tc.in)
		}
		if got != tc.want {
			t.Fatalf("ParseGitHubReleaseURL(%q)=%+v, want %+v", tc.in, got, tc.want)
		}
	}
	bad := []string{
		"",
		"https://github.com/o/r",
		"https://github.com/o/r/releases/tag/v1",
		"https://gitlab.com/o/r/releases/download/v1/a",
		"https://api.github.com/repos/o/r/releases/assets/1",
		"file:///tmp/x",
	}
	for _, uri := range bad {
		if _, ok := ParseGitHubReleaseURL(uri); ok {
			t.Errorf("ParseGitHubReleaseURL(%q) ok=true, want false", uri)
		}
	}
}

func TestNeedsIngestGitHubUsesAPICreds(t *testing.T) {
	t.Setenv("GH_PAT", "secret-pat")
	path := t.TempDir() + "/c.yaml"
	if err := os.WriteFile(path, []byte(`
artifact_credentials:
  api.github.com:
    type: bearer
    token_env: GH_PAT
`), 0o600); err != nil {
		t.Fatal(err)
	}
	creds, err := LoadCredentials(path)
	if err != nil {
		t.Fatal(err)
	}
	svc := New(store.NewMemory(nil), newMemObjects("b"), creds, ModeCredentialedOnly, nil)
	uri := "https://github.com/org/repo/releases/download/v1/strat"
	if !svc.NeedsIngest(uri) {
		t.Fatal("github browser URL with api.github.com cred should ingest")
	}
	svcNo := New(store.NewMemory(nil), newMemObjects("b"), mustEmptyCreds(t), ModeCredentialedOnly, nil)
	if svcNo.NeedsIngest(uri) {
		t.Fatal("without api.github.com cred, browser URL should not ingest")
	}
}

func TestGitHubAssetDownloadStripsAuthOnCrossHostRedirect(t *testing.T) {
	const body = "private-asset-bytes"
	var objectsSawAuth atomic.Bool
	var apiGotBearer atomic.Bool

	objects := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			objectsSawAuth.Store(true)
		}
		_, _ = w.Write([]byte(body))
	}))
	defer objects.Close()

	// Distinct hostname (like objects.githubusercontent.com). Go strips
	// Authorization when Hostname() differs — same 127.0.0.1:port would keep it.
	const objectsHost = "objects.githubusercontent.test"
	objectsAddr := objects.Listener.Addr().String()

	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/releases/tags/v1"):
			if !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
				http.Error(w, "no auth", http.StatusUnauthorized)
				return
			}
			_ = json.NewEncoder(w).Encode(ghRelease{Assets: []ghAsset{
				{ID: 42, Name: "strat"},
			}})
		case strings.HasSuffix(r.URL.Path, "/releases/assets/42"):
			apiGotBearer.Store(strings.HasPrefix(r.Header.Get("Authorization"), "Bearer secret-pat"))
			if r.Header.Get("Accept") != "application/octet-stream" {
				http.Error(w, "bad accept", http.StatusUnsupportedMediaType)
				return
			}
			http.Redirect(w, r, "http://"+objectsHost+"/blob", http.StatusFound)
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	creds := &Credentials{byHost: map[string]HostCredential{
		githubAPIHost: {Type: CredBearer, Token: "secret-pat"},
	}}
	svc := New(store.NewMemory(nil), newMemObjects("artifacts"), creds, ModeCredentialedOnly, nil)
	svc.GitHubAPIBase = api.URL
	svc.Client = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				if strings.HasPrefix(addr, objectsHost+":") {
					addr = objectsAddr
				}
				var d net.Dialer
				return d.DialContext(ctx, network, addr)
			},
		},
	}

	tmp, err := os.CreateTemp(t.TempDir(), "gh-asset-*")
	if err != nil {
		t.Fatal(err)
	}
	defer tmp.Close()

	ghRef := GitHubReleaseRef{Owner: "org", Repo: "repo", Tag: "v1", Asset: "strat"}
	if err := svc.downloadGitHubRelease(context.Background(), ghRef, creds.byHost[githubAPIHost], tmp); err != nil {
		t.Fatal(err)
	}
	if !apiGotBearer.Load() {
		t.Fatal("asset API request missing Bearer")
	}
	if objectsSawAuth.Load() {
		t.Fatal("Authorization must not be forwarded to objects host after 302")
	}
	got, err := os.ReadFile(tmp.Name())
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("body = %q", got)
	}
}

func TestGitHubIngestEndToEndViaPipeline(t *testing.T) {
	const body = "release-bin"
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.Contains(r.URL.Path, "/releases/tags/"):
			_ = json.NewEncoder(w).Encode(ghRelease{Assets: []ghAsset{{ID: 7, Name: "bin"}}})
		case strings.Contains(r.URL.Path, "/releases/assets/7"):
			if r.Header.Get("Authorization") != "Bearer pat" {
				http.Error(w, "auth", http.StatusUnauthorized)
				return
			}
			_, _ = w.Write([]byte(body))
		default:
			http.NotFound(w, r)
		}
	}))
	defer api.Close()

	creds := &Credentials{byHost: map[string]HostCredential{
		githubAPIHost: {Type: CredBearer, Token: "pat"},
	}}
	st := store.NewMemory(nil)
	objs := newMemObjects("artifacts")
	svc := New(st, objs, creds, ModeCredentialedOnly, nil)
	svc.GitHubAPIBase = api.URL

	sum := sha256.Sum256([]byte(body))
	digest := "sha256:" + hex.EncodeToString(sum[:])
	ref := &pb.ArtifactRef{
		Name: "s", Version: "v1", Digest: digest,
		Uri: "https://github.com/acme/app/releases/download/v1/bin",
	}
	_ = st.RegisterArtifactRecord(&store.ArtifactRecord{Ref: ref, State: store.ArtifactStatePending})
	if err := svc.Run(context.Background(), ref); err != nil {
		t.Fatal(err)
	}
	rec, _ := st.GetArtifactRecord("s", "v1")
	if rec.State != store.ArtifactStateReady {
		t.Fatalf("state=%s reason=%s", rec.State, rec.StateReason)
	}
	if !strings.HasPrefix(rec.Ref.GetUri(), "s3://") {
		t.Fatalf("uri=%q", rec.Ref.GetUri())
	}
}

func TestGitHubAssetIDCache(t *testing.T) {
	var hits atomic.Int32
	api := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "/releases/tags/") {
			hits.Add(1)
			_ = json.NewEncoder(w).Encode(ghRelease{Assets: []ghAsset{{ID: 9, Name: "a"}}})
			return
		}
		_, _ = w.Write([]byte("x"))
	}))
	defer api.Close()

	creds := &Credentials{byHost: map[string]HostCredential{
		githubAPIHost: {Type: CredBearer, Token: "t"},
	}}
	svc := New(store.NewMemory(nil), newMemObjects("b"), creds, ModeCredentialedOnly, nil)
	svc.GitHubAPIBase = api.URL
	ref := GitHubReleaseRef{Owner: "o", Repo: "r", Tag: "v1", Asset: "a"}

	id1, err := svc.resolveGitHubAssetID(context.Background(), ref, "t")
	if err != nil || id1 != 9 {
		t.Fatalf("id1=%d err=%v", id1, err)
	}
	id2, err := svc.resolveGitHubAssetID(context.Background(), ref, "t")
	if err != nil || id2 != 9 {
		t.Fatalf("id2=%d err=%v", id2, err)
	}
	if hits.Load() != 1 {
		t.Fatalf("tag API hits=%d, want 1 (cache)", hits.Load())
	}
}

func TestGitHubAssetCacheExpiry(t *testing.T) {
	c := newAssetIDCache(time.Minute)
	now := time.Unix(1000, 0)
	c.now = func() time.Time { return now }
	ref := GitHubReleaseRef{Owner: "o", Repo: "r", Tag: "t", Asset: "a"}
	c.put(ref, 1)
	if id, ok := c.get(ref); !ok || id != 1 {
		t.Fatalf("get = %d %v", id, ok)
	}
	now = now.Add(2 * time.Minute)
	if _, ok := c.get(ref); ok {
		t.Fatal("expected expired")
	}
}
