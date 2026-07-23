package artifact

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

type countingResolver struct {
	n   atomic.Int32
	url string
	err error
}

func (c *countingResolver) Resolve(_ context.Context, ref *pb.ArtifactRef) (Source, error) {
	c.n.Add(1)
	if c.err != nil {
		return Source{}, c.err
	}
	u := c.url
	if u == "" {
		u = "https://presigned.example/" + ref.GetVersion()
	}
	return Source{URL: u, ExpiresAt: time.Now().Add(5 * time.Minute)}, nil
}

type captureFetcher struct {
	uris []string
}

func (c *captureFetcher) Fetch(_ context.Context, ref *pb.ArtifactRef, _ string) error {
	c.uris = append(c.uris, ref.GetUri())
	return nil
}

func TestResolvingFetcherResolvesEachAttempt(t *testing.T) {
	res := &countingResolver{url: "https://presigned.example/v1"}
	cap := &captureFetcher{}
	f := ResolvingFetcher{Resolver: res, Inner: cap}
	ref := &pb.ArtifactRef{Name: "s", Version: "v1", Uri: "s3://artifacts/s/v1/aaa"}

	for i := 0; i < 3; i++ {
		if err := f.Fetch(context.Background(), ref, "dest"); err != nil {
			t.Fatalf("fetch %d: %v", i, err)
		}
	}
	if res.n.Load() != 3 {
		t.Fatalf("resolve calls = %d, want 3 (per fetch attempt)", res.n.Load())
	}
	for i, uri := range cap.uris {
		if uri != "https://presigned.example/v1" {
			t.Fatalf("attempt %d fetched %q, want presigned https", i, uri)
		}
	}
}

func TestResolvingFetcherPropagatesResolveError(t *testing.T) {
	res := &countingResolver{err: errors.New("expired")}
	f := ResolvingFetcher{Resolver: res, Inner: &captureFetcher{}}
	err := f.Fetch(context.Background(), &pb.ArtifactRef{Uri: "s3://b/k", Name: "s", Version: "v1"}, "dest")
	if err == nil {
		t.Fatal("expected error")
	}
}

func TestPassthroughResolver(t *testing.T) {
	src, err := PassthroughResolver{}.Resolve(context.Background(), &pb.ArtifactRef{Uri: "file:///tmp/x"})
	if err != nil {
		t.Fatal(err)
	}
	if src.URL != "file:///tmp/x" {
		t.Fatalf("url = %q", src.URL)
	}
}

type fakeArtifactSourceClient struct {
	calls int
	url   string
	err   error
}

func (f *fakeArtifactSourceClient) ResolveArtifactSource(context.Context, *connect.Request[pb.ResolveArtifactSourceRequest]) (*connect.Response[pb.ResolveArtifactSourceResponse], error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	return connect.NewResponse(&pb.ResolveArtifactSourceResponse{
		Url:       f.url,
		ExpiresAt: timestamppb.New(time.Now().Add(5 * time.Minute)),
	}), nil
}

func TestCPResolverPassthroughNonS3(t *testing.T) {
	client := &fakeArtifactSourceClient{url: "https://should-not-call"}
	r := NewCPResolver(client)
	src, err := r.Resolve(context.Background(), &pb.ArtifactRef{Uri: "https://github.com/o/r/bin"})
	if err != nil {
		t.Fatal(err)
	}
	if src.URL != "https://github.com/o/r/bin" {
		t.Fatalf("url = %q", src.URL)
	}
	if client.calls != 0 {
		t.Fatalf("unexpected RPC calls: %d", client.calls)
	}
}

func TestCPResolverCallsRPCForS3(t *testing.T) {
	client := &fakeArtifactSourceClient{url: "https://presigned.example/obj"}
	r := NewCPResolver(client)
	src, err := r.Resolve(context.Background(), &pb.ArtifactRef{
		Name: "s", Version: "v1", Uri: "s3://artifacts/s/v1/aaa",
	})
	if err != nil {
		t.Fatal(err)
	}
	if src.URL != "https://presigned.example/obj" {
		t.Fatalf("url = %q", src.URL)
	}
	if client.calls != 1 {
		t.Fatalf("calls = %d", client.calls)
	}
}
