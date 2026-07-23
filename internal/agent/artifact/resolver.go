package artifact

import (
	"context"
	"fmt"
	"time"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/artifacturi"
)

// ArtifactSourceClient is the ResolveArtifactSource RPC surface. Satisfied by
// strategyplatformv1connect.AgentServiceClient.
type ArtifactSourceClient interface {
	ResolveArtifactSource(context.Context, *connect.Request[pb.ResolveArtifactSourceRequest]) (*connect.Response[pb.ResolveArtifactSourceResponse], error)
}

// Source is a concrete fetch location after URI resolution. For http(s) and
// file:// this is identity; for s3:// the control plane returns a short-lived
// presigned HTTPS URL.
type Source struct {
	URL       string
	ExpiresAt time.Time
	// Headers reserved for a future per-fetch header delivery path (design
	// §1.1 C-weak). Must not be enabled without an explicit decision.
}

// SourceResolver turns an ArtifactRef into a fetchable Source. Resolve must be
// called on every fetch attempt — presigned URLs expire, and retries after
// backoff must not reuse a stale URL.
type SourceResolver interface {
	Resolve(ctx context.Context, ref *pb.ArtifactRef) (Source, error)
}

// PassthroughResolver returns ref.uri unchanged. Used for file://, unit tests,
// and offline situations where no control-plane round trip is needed.
type PassthroughResolver struct{}

// Resolve implements SourceResolver.
func (PassthroughResolver) Resolve(_ context.Context, ref *pb.ArtifactRef) (Source, error) {
	if ref == nil || ref.GetUri() == "" {
		return Source{}, fmt.Errorf("resolve: empty artifact uri")
	}
	return Source{URL: ref.GetUri()}, nil
}

// CPResolver resolves s3:// URIs via the control plane's ResolveArtifactSource
// RPC and passes all other schemes through unchanged.
type CPResolver struct {
	Client ArtifactSourceClient
}

// NewCPResolver returns a resolver that dials ResolveArtifactSource for s3://.
func NewCPResolver(client ArtifactSourceClient) *CPResolver {
	return &CPResolver{Client: client}
}

// Resolve implements SourceResolver.
func (r *CPResolver) Resolve(ctx context.Context, ref *pb.ArtifactRef) (Source, error) {
	if ref == nil || ref.GetUri() == "" {
		return Source{}, fmt.Errorf("resolve: empty artifact uri")
	}
	if !artifacturi.IsS3(ref.GetUri()) {
		return Source{URL: ref.GetUri()}, nil
	}
	if r == nil || r.Client == nil {
		return Source{}, fmt.Errorf("resolve: s3:// uri %q requires a control-plane client", ref.GetUri())
	}
	if ref.GetName() == "" || ref.GetVersion() == "" {
		return Source{}, fmt.Errorf("resolve: s3:// artifact requires name and version")
	}
	resp, err := r.Client.ResolveArtifactSource(ctx, connect.NewRequest(&pb.ResolveArtifactSourceRequest{
		Name:    ref.GetName(),
		Version: ref.GetVersion(),
	}))
	if err != nil {
		return Source{}, fmt.Errorf("resolve %s@%s: %w", ref.GetName(), ref.GetVersion(), err)
	}
	src := Source{URL: resp.Msg.GetUrl()}
	if exp := resp.Msg.GetExpiresAt(); exp != nil {
		src.ExpiresAt = exp.AsTime()
	}
	if src.URL == "" {
		return Source{}, fmt.Errorf("resolve %s@%s: empty url from control plane", ref.GetName(), ref.GetVersion())
	}
	return src, nil
}

// ResolvingFetcher resolves the ArtifactRef URI before delegating to Inner.
// Production wires CPResolver + SchemeFetcher so s3:// becomes a presigned
// https:// URL that HTTPFetcher can download unchanged.
type ResolvingFetcher struct {
	Resolver SourceResolver
	Inner    Fetcher
}

// Fetch resolves then fetches. Resolve runs on every call so retries after
// backoff get a fresh presigned URL.
func (f ResolvingFetcher) Fetch(ctx context.Context, ref *pb.ArtifactRef, dest string) error {
	resolver := f.Resolver
	if resolver == nil {
		resolver = PassthroughResolver{}
	}
	if f.Inner == nil {
		return fmt.Errorf("resolving fetcher: no inner fetcher")
	}
	src, err := resolver.Resolve(ctx, ref)
	if err != nil {
		return err
	}
	resolved := *ref
	resolved.Uri = src.URL
	return f.Inner.Fetch(ctx, &resolved, dest)
}
