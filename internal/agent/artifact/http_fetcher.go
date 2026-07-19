package artifact

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

// HTTPFetcher downloads artifact bytes over http(s) — e.g. a GitHub Releases
// asset URL — into dest. It does NOT verify integrity; Manager.Verify re-hashes
// the written file against ref.digest before the release is switched in, so a
// corrupted or tampered download is rejected there. Redirects (GitHub Releases
// bounces to objects.githubusercontent.com) are followed by the default client.
type HTTPFetcher struct {
	// Client is the HTTP client used for downloads. A nil client falls back to
	// http.DefaultClient (no timeout); NewHTTPFetcher sets a sensible timeout.
	Client *http.Client
}

// NewHTTPFetcher returns an HTTPFetcher with a 10-minute download timeout,
// enough for large binaries on slow links while still bounding a hung transfer.
func NewHTTPFetcher() HTTPFetcher {
	return HTTPFetcher{Client: &http.Client{Timeout: 10 * time.Minute}}
}

// Fetch GETs ref.uri and streams the body to dest.
func (f HTTPFetcher) Fetch(ctx context.Context, ref *pb.ArtifactRef, dest string) error {
	client := f.Client
	if client == nil {
		client = http.DefaultClient
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, ref.GetUri(), nil)
	if err != nil {
		return fmt.Errorf("http fetcher: new request for %q: %w", ref.GetUri(), err)
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("http fetcher: get %q: %w", ref.GetUri(), err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("http fetcher: get %q: unexpected status %s", ref.GetUri(), resp.Status)
	}
	out, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("http fetcher: create dest %s: %w", dest, err)
	}
	defer out.Close()
	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("http fetcher: copy %q -> %s: %w", ref.GetUri(), dest, err)
	}
	return nil
}
