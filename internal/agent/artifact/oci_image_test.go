package artifact

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/mutate"
	"github.com/google/go-containerregistry/pkg/v1/tarball"
)

func fileLayer(t *testing.T, files map[string]string) v1.Layer {
	t.Helper()
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	for name, body := range files {
		hdr := &tar.Header{Name: name, Mode: 0o755, Size: int64(len(body))}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	raw := buf.Bytes()
	layer, err := tarball.LayerFromOpener(func() (io.ReadCloser, error) {
		return io.NopCloser(bytes.NewReader(raw)), nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return layer
}

func writeDockerArchive(t *testing.T, arch string, files map[string]string) (path, digest string) {
	t.Helper()
	cfg := v1.Config{
		Entrypoint: []string{"/hello"},
		Cmd:        []string{"--ok"},
		Env:        []string{"FOO=bar"},
		WorkingDir: "/app",
		User:       "65532",
	}
	img, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{
		OS:           "linux",
		Architecture: arch,
		Config:       cfg,
	})
	if err != nil {
		t.Fatal(err)
	}
	img, err = mutate.Append(img, mutate.Addendum{Layer: fileLayer(t, files)})
	if err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), "img.tar")
	tag, err := name.NewTag("strategon/test:v1")
	if err != nil {
		t.Fatal(err)
	}
	if err := tarball.WriteToFile(path, tag, img); err != nil {
		t.Fatal(err)
	}
	sum, err := fileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, "sha256:" + sum
}

func TestDownloadOCIUnpackAndVerify(t *testing.T) {
	path, digest := writeDockerArchive(t, runtime.GOARCH, map[string]string{"hello": "#!/bin/true\n"})
	base := t.TempDir()
	mgr := NewManager(base, LocalFetcher{})
	ref := &pb.ArtifactRef{
		Type: pb.ArtifactType_ARTIFACT_TYPE_OCI_IMAGE, Name: "s", Version: "v1",
		Digest: digest, Uri: "file://" + path,
	}
	if err := mgr.Download(context.Background(), "s", ref, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mgr.ImageTarPath("s", "v1")); !os.IsNotExist(err) {
		t.Fatal("image.tar should be deleted after unpack")
	}
	if err := mgr.Verify("s", ref); err != nil {
		t.Fatal(err)
	}
	meta, err := mgr.readOCIMeta("s", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if meta.User != "65532" || len(meta.Entrypoint) != 1 || meta.Entrypoint[0] != "/hello" {
		t.Fatalf("meta = %+v", meta)
	}
	if _, err := os.Stat(filepath.Join(mgr.RootfsPath("s", "v1"), "hello")); err != nil {
		t.Fatalf("hello missing: %v", err)
	}
	// Second download is a no-op (HasVerifiedRelease).
	if err := mgr.Download(context.Background(), "s", ref, nil); err != nil {
		t.Fatal(err)
	}
}

func TestDownloadOCIPlatformMismatch(t *testing.T) {
	other := "arm64"
	if runtime.GOARCH == "arm64" {
		other = "amd64"
	}
	path, digest := writeDockerArchive(t, other, map[string]string{"x": "x"})
	mgr := NewManager(t.TempDir(), LocalFetcher{})
	ref := &pb.ArtifactRef{
		Type: pb.ArtifactType_ARTIFACT_TYPE_OCI_IMAGE, Version: "v1",
		Digest: digest, Uri: "file://" + path,
	}
	err := mgr.Download(context.Background(), "s", ref, nil)
	if err == nil {
		t.Fatal("expected platform mismatch")
	}
}

func TestDownloadOCIDigestMismatch(t *testing.T) {
	path, _ := writeDockerArchive(t, runtime.GOARCH, map[string]string{"x": "x"})
	mgr := NewManager(t.TempDir(), LocalFetcher{})
	ref := &pb.ArtifactRef{
		Type: pb.ArtifactType_ARTIFACT_TYPE_OCI_IMAGE, Version: "v1",
		Digest: "sha256:" + hex.EncodeToString(sha256.New().Sum(nil)),
		Uri:    "file://" + path,
	}
	if err := mgr.Download(context.Background(), "s", ref, nil); err == nil {
		t.Fatal("expected digest mismatch")
	}
}

func TestParseUser(t *testing.T) {
	uid, gid := ParseUser("65532")
	if uid != 65532 || gid != 65532 {
		t.Fatalf("%d %d", uid, gid)
	}
	uid, gid = ParseUser("1:2")
	if uid != 1 || gid != 2 {
		t.Fatalf("%d %d", uid, gid)
	}
	uid, gid = ParseUser("nobody")
	if uid != 0 || gid != 0 {
		t.Fatalf("name should fall back to 0")
	}
}

func TestGCReleasesKeepsNamedAndNewest(t *testing.T) {
	base := t.TempDir()
	mgr := NewManager(base, LocalFetcher{})
	mgr.ReleaseRetention = 3
	for _, v := range []string{"v1", "v2", "v3", "v4", "v5"} {
		if err := os.MkdirAll(mgr.ReleaseDir("s", v), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := mgr.GCReleases("s", []string{"v5", "v4"}); err != nil {
		t.Fatal(err)
	}
	exists := func(v string) bool {
		_, err := os.Stat(mgr.ReleaseDir("s", v))
		return err == nil
	}
	if !exists("v5") || !exists("v4") {
		t.Fatal("keep set deleted")
	}
	// retention 3, keep 2 → 1 extra newest among v1–v3. Creation order makes
	// v3 the newest extra; v1 and v2 should go.
	if exists("v1") || exists("v2") {
		t.Fatal("old extras should be gc'd")
	}
}

func TestSafeTarNameRejectsSlip(t *testing.T) {
	if _, err := safeTarName("../etc/passwd"); err == nil {
		t.Fatal("expected slip")
	}
	got, err := safeTarName("./usr/bin/true")
	if err != nil || got != "usr/bin/true" {
		t.Fatalf("%q %v", got, err)
	}
}
