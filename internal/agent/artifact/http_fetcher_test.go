package artifact

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

func TestHTTPFetcherDownloads(t *testing.T) {
	const body = "#!/bin/sh\necho hi\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/asset" {
			_, _ = w.Write([]byte(body))
			return
		}
		http.NotFound(w, r)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "bin")
	f := NewHTTPFetcher()
	if err := f.Fetch(context.Background(), &pb.ArtifactRef{Uri: srv.URL + "/asset"}, dest); err != nil {
		t.Fatalf("fetch: %v", err)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != body {
		t.Fatalf("downloaded %q, want %q", got, body)
	}
}

func TestHTTPFetcherNon200(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	}))
	defer srv.Close()

	dest := filepath.Join(t.TempDir(), "bin")
	err := NewHTTPFetcher().Fetch(context.Background(), &pb.ArtifactRef{Uri: srv.URL + "/missing"}, dest)
	if err == nil {
		t.Fatal("expected error on 404")
	}
	if !strings.Contains(err.Error(), "not found (404)") {
		t.Fatalf("error = %v, want readable 404 classification", err)
	}
}

func TestHTTPFetcherStatusClassification(t *testing.T) {
	cases := []struct {
		code int
		want string
	}{
		{http.StatusUnauthorized, "unauthorized (401)"},
		{http.StatusForbidden, "forbidden (403)"},
		{http.StatusNotFound, "not found (404)"},
		{http.StatusBadGateway, "unexpected status"},
	}
	for _, tc := range cases {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(tc.code)
		}))
		err := NewHTTPFetcher().Fetch(context.Background(), &pb.ArtifactRef{Uri: srv.URL}, filepath.Join(t.TempDir(), "bin"))
		srv.Close()
		if err == nil {
			t.Fatalf("status %d: expected error", tc.code)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("status %d: error = %v, want substring %q", tc.code, err, tc.want)
		}
	}
}

// fakeFetcher records which fetcher a SchemeFetcher dispatched to.
type fakeFetcher struct{ hits *[]string; tag string }

func (f fakeFetcher) Fetch(_ context.Context, _ *pb.ArtifactRef, _ string) error {
	*f.hits = append(*f.hits, f.tag)
	return nil
}

func TestSchemeFetcherRouting(t *testing.T) {
	var hits []string
	s := SchemeFetcher{Local: fakeFetcher{&hits, "local"}, HTTP: fakeFetcher{&hits, "http"}}
	cases := map[string]string{
		"https://github.com/o/r/releases/download/v1/bin": "http",
		"http://example.com/bin":                          "http",
		"file:///tmp/bin":                                 "local",
		"/tmp/bin":                                        "local",
	}
	for uri, want := range cases {
		hits = nil
		if err := s.Fetch(context.Background(), &pb.ArtifactRef{Uri: uri}, "dest"); err != nil {
			t.Fatalf("fetch %q: %v", uri, err)
		}
		if len(hits) != 1 || hits[0] != want {
			t.Fatalf("uri %q routed to %v, want %q", uri, hits, want)
		}
	}
}
