package artifact

import (
	"context"
	"fmt"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/artifacturi"
)

// SchemeFetcher routes a Fetch to a concrete fetcher by the ArtifactRef URI
// scheme: http(s) -> HTTP, file:// or a bare absolute path -> Local. This lets a
// single agent pull binaries from GitHub Releases and from local paths without
// the reconciler knowing which transport is in play.
type SchemeFetcher struct {
	Local Fetcher
	HTTP  Fetcher
}

// NewDefaultFetcher returns a SchemeFetcher wired with the production fetchers:
// a local-filesystem copier and an http(s) downloader.
func NewDefaultFetcher() SchemeFetcher {
	return SchemeFetcher{Local: LocalFetcher{}, HTTP: NewHTTPFetcher()}
}

// Fetch dispatches by URI scheme.
func (s SchemeFetcher) Fetch(ctx context.Context, ref *pb.ArtifactRef, dest string) error {
	if artifacturi.IsHTTP(ref.GetUri()) {
		if s.HTTP == nil {
			return fmt.Errorf("fetcher: no http fetcher configured for %q", ref.GetUri())
		}
		return s.HTTP.Fetch(ctx, ref, dest)
	}
	if s.Local == nil {
		return fmt.Errorf("fetcher: no local fetcher configured for %q", ref.GetUri())
	}
	return s.Local.Fetch(ctx, ref, dest)
}
