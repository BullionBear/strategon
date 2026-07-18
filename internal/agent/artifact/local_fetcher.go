package artifact

import (
	"context"
	"fmt"
	"io"
	"os"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/artifacturi"
)

// LocalFetcher copies artifact bytes from a local source path encoded in the
// ArtifactRef URI ("file:///abs/path"). It stands in for the S3/MinIO backend
// (deferred) so the whole deploy path is exercisable in tests and single-host
// runs.
type LocalFetcher struct{}

// Fetch copies the file referenced by ref.uri to dest.
func (LocalFetcher) Fetch(ctx context.Context, ref *pb.ArtifactRef, dest string) error {
	src, err := artifacturi.ResolveLocal(ref.GetUri())
	if err != nil {
		return fmt.Errorf("local fetcher: %w", err)
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
