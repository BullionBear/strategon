package artifact

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

// LocalFetcher copies artifact bytes from a local source path encoded in the
// ArtifactRef URI ("file:///abs/path"). It stands in for the S3/MinIO backend
// (deferred) so the whole deploy path is exercisable in tests and single-host
// runs.
type LocalFetcher struct{}

// Fetch copies the file referenced by ref.uri to dest.
func (LocalFetcher) Fetch(ctx context.Context, ref *pb.ArtifactRef, dest string) error {
	uri := ref.GetUri()
	src := strings.TrimPrefix(uri, "file://")
	if src == uri && !strings.HasPrefix(uri, "/") {
		return fmt.Errorf("local fetcher: unsupported uri %q (want file:// or absolute path)", uri)
	}
	in, err := os.Open(src)
	if err != nil {
		return fmt.Errorf("open source %s: %w", src, err)
	}
	defer in.Close()
	out, err := os.Create(dest)
	if err != nil {
		return fmt.Errorf("create dest %s: %w", dest, err)
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return fmt.Errorf("copy: %w", err)
	}
	return nil
}
