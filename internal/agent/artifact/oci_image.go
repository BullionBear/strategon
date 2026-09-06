package artifact

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

func unpackOCIImage(ctx context.Context, tarPath, destRootfs string, wantDigest string) (*OCIMeta, error) {
	img, plat, err := loadImage(ctx, tarPath)
	if err != nil {
		return nil, err
	}
	if err := ensureUnpackSpace(destRootfs, img); err != nil {
		return nil, err
	}
	tmp := destRootfs + ".tmp"
	_ = os.RemoveAll(tmp)
	if err := os.MkdirAll(tmp, 0o755); err != nil {
		return nil, err
	}
	defer os.RemoveAll(tmp)

	rc := mutate.Extract(img)
	defer rc.Close()
	if err := extractFlattened(ctx, rc, tmp); err != nil {
		return nil, fmt.Errorf("extract rootfs: %w", err)
	}

	cfg, err := img.ConfigFile()
	if err != nil {
		return nil, fmt.Errorf("image config: %w", err)
	}
	meta := &OCIMeta{
		SchemaVersion: ociSchema,
		Digest:        wantDigest,
		WorkingDir:    cfg.Config.WorkingDir,
		User:          cfg.Config.User,
		StopSignal:    cfg.Config.StopSignal,
		Platform:      plat.String(),
	}
	if cfg.Config.Entrypoint != nil {
		meta.Entrypoint = append([]string(nil), cfg.Config.Entrypoint...)
	}
	if cfg.Config.Cmd != nil {
		meta.Cmd = append([]string(nil), cfg.Config.Cmd...)
	}
	if cfg.Config.Env != nil {
		meta.Env = append([]string(nil), cfg.Config.Env...)
	}

	_ = os.RemoveAll(destRootfs)
	if err := os.Rename(tmp, destRootfs); err != nil {
		return nil, fmt.Errorf("rename rootfs: %w", err)
	}
	return meta, nil
}

func loadImage(ctx context.Context, tarPath string) (v1.Image, v1.Platform, error) {
	if err := ctx.Err(); err != nil {
		return nil, v1.Platform{}, err
	}
	kind, err := peekArchiveKind(tarPath)
	if err != nil {
		return nil, v1.Platform{}, err
	}
	want := v1.Platform{OS: "linux", Architecture: runtime.GOARCH}
	switch kind {
	case "oci":
		return loadOCILayoutTar(ctx, tarPath, want)
	default:
		return loadDockerTar(tarPath, want)
	}
}

func peekArchiveKind(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return "docker", nil
		}
		if err != nil {
			return "", err
		}
		name := strings.TrimPrefix(hdr.Name, "./")
		if name == "index.json" {
			return "oci", nil
		}
		if name == "manifest.json" {
			return "docker", nil
		}
	}
}

func loadDockerTar(path string, want v1.Platform) (v1.Image, v1.Platform, error) {
	tags, err := dockerRepoTags(path)
	if err != nil {
		return nil, v1.Platform{}, err
	}
	if len(tags) == 0 {
		img, err := tarball.ImageFromPath(path, nil)
		if err != nil {
			return nil, v1.Platform{}, fmt.Errorf("docker archive: %w", err)
		}
		return matchPlatform(img, want, nil)
	}
	var seen []string
	for _, tag := range tags {
		ref, err := name.NewTag(tag, name.WeakValidation)
		if err != nil {
			continue
		}
		img, err := tarball.ImageFromPath(path, &ref)
		if err != nil {
			continue
		}
		got, err := imagePlatform(img)
		if err != nil {
			continue
		}
		seen = append(seen, got.String())
		if platformOK(got, want) {
			return img, got, nil
		}
	}
	// Last resort: first image if it has empty platform.
	img, err := tarball.ImageFromPath(path, nil)
	if err != nil {
		return nil, v1.Platform{}, fmt.Errorf("docker archive: no image for linux/%s (have %s)", want.Architecture, strings.Join(seen, ", "))
	}
	return matchPlatform(img, want, seen)
}

func dockerRepoTags(path string) ([]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		if strings.TrimPrefix(hdr.Name, "./") != "manifest.json" {
			continue
		}
		var mf []struct {
			RepoTags []string `json:"RepoTags"`
		}
		b, err := io.ReadAll(tr)
		if err != nil {
			return nil, err
		}
		if err := json.Unmarshal(b, &mf); err != nil {
			return nil, err
		}
		var tags []string
		for _, e := range mf {
			tags = append(tags, e.RepoTags...)
		}
		return tags, nil
	}
}

func loadOCILayoutTar(ctx context.Context, tarPath string, want v1.Platform) (v1.Image, v1.Platform, error) {
	tmp, err := os.MkdirTemp(filepath.Dir(tarPath), "oci-layout-*")
	if err != nil {
		return nil, v1.Platform{}, err
	}
	defer os.RemoveAll(tmp)
	f, err := os.Open(tarPath)
	if err != nil {
		return nil, v1.Platform{}, err
	}
	if err := extractFlattened(ctx, f, tmp); err != nil {
		f.Close()
		return nil, v1.Platform{}, fmt.Errorf("extract oci layout: %w", err)
	}
	f.Close()

	idx, err := layout.ImageIndexFromPath(tmp)
	if err != nil {
		return nil, v1.Platform{}, fmt.Errorf("oci layout: %w", err)
	}
	mf, err := idx.IndexManifest()
	if err != nil {
		return nil, v1.Platform{}, err
	}
	var seen []string
	for _, d := range mf.Manifests {
		if d.MediaType.IsIndex() {
			continue
		}
		p := v1.Platform{OS: "linux"}
		if d.Platform != nil {
			p = *d.Platform
		}
		seen = append(seen, p.String())
		if !platformOK(p, want) {
			continue
		}
		img, err := idx.Image(d.Digest)
		if err != nil {
			return nil, v1.Platform{}, err
		}
		return img, p, nil
	}
	return nil, v1.Platform{}, fmt.Errorf("oci layout: no image for linux/%s (have %s)", want.Architecture, strings.Join(seen, ", "))
}

func imagePlatform(img v1.Image) (v1.Platform, error) {
	cf, err := img.ConfigFile()
	if err != nil {
		return v1.Platform{}, err
	}
	return v1.Platform{OS: cf.OS, Architecture: cf.Architecture, Variant: cf.Variant}, nil
}

func platformOK(got, want v1.Platform) bool {
	if got.OS != "" && !strings.EqualFold(got.OS, want.OS) {
		return false
	}
	if got.Architecture != "" && !strings.EqualFold(got.Architecture, want.Architecture) {
		return false
	}
	return true
}

func matchPlatform(img v1.Image, want v1.Platform, seen []string) (v1.Image, v1.Platform, error) {
	got, err := imagePlatform(img)
	if err != nil {
		return nil, v1.Platform{}, err
	}
	if platformOK(got, want) {
		return img, got, nil
	}
	if len(seen) == 0 {
		seen = []string{got.String()}
	}
	return nil, v1.Platform{}, fmt.Errorf("image platform %s does not match host linux/%s (have %s)", got.String(), want.Architecture, strings.Join(seen, ", "))
}

func ensureUnpackSpace(dest string, img v1.Image) error {
	need, err := estimateUnpackBytes(img)
	if err != nil {
		return err
	}
	avail, err := availableBytes(filepath.Dir(dest))
	if err != nil {
		return nil // best-effort: cannot statfs
	}
	if avail < uint64(need) {
		return fmt.Errorf("not enough disk to unpack image: need ~%d bytes, have %d", need, avail)
	}
	return nil
}

func estimateUnpackBytes(img v1.Image) (int64, error) {
	layers, err := img.Layers()
	if err != nil {
		return 0, err
	}
	var compressed int64
	for _, l := range layers {
		n, err := l.Size()
		if err != nil {
			return 0, err
		}
		compressed += n
	}
	// gzip typically 2–4x; 3x plus slack. Prefer uncompressed when cheap.
	var uncompressed int64
	for _, l := range layers {
		r, err := l.Uncompressed()
		if err != nil {
			return compressed*3 + 64<<20, nil
		}
		n, err := io.Copy(io.Discard, r)
		r.Close()
		if err != nil {
			return compressed*3 + 64<<20, nil
		}
		uncompressed += n
	}
	if uncompressed > 0 {
		return uncompressed + 64<<20, nil
	}
	return compressed*3 + 64<<20, nil
}

func availableBytes(path string) (uint64, error) {
	return fsAvail(path)
}
