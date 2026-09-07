package artifact

import (
	"archive/tar"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/google/go-containerregistry/pkg/name"
	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
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

// otherArch is an architecture the host is not, so a built image is a genuine
// platform mismatch wherever the suite runs.
func otherArch() string {
	if runtime.GOARCH == "arm64" {
		return "amd64"
	}
	return "arm64"
}

func TestDownloadOCIPlatformMismatch(t *testing.T) {
	path, digest := writeDockerArchive(t, otherArch(), map[string]string{"x": "x"})
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

// writeOCILayoutArchive produces a flat OCI layout tar: index.json points
// directly at the image manifest. Classic `docker save` (graph driver) emits
// a docker archive instead; Engine 25+ with the containerd snapshotter emits
// the nested shape — see writeNestedOCILayoutArchive.
func writeOCILayoutArchive(t *testing.T, arch string, files map[string]string) (path, digest string) {
	t.Helper()
	img, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{
		OS:           "linux",
		Architecture: arch,
		Config:       v1.Config{Entrypoint: []string{"/hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	img, err = mutate.Append(img, mutate.Addendum{Layer: fileLayer(t, files)})
	if err != nil {
		t.Fatal(err)
	}
	idx := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{
		Add: img,
		Descriptor: v1.Descriptor{
			Platform: &v1.Platform{OS: "linux", Architecture: arch},
		},
	})
	dir := t.TempDir()
	if _, err := layout.Write(dir, idx); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), "oci.tar")
	tarDir(t, dir, path)
	sum, err := fileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, "sha256:" + sum
}

// writeNestedOCILayoutArchive mirrors containerd-store `docker save`:
// top-level index → nested index → linux/<arch> manifest plus an
// unknown/unknown attestation descriptor that platformOK must skip.
func writeNestedOCILayoutArchive(t *testing.T, arch string, files map[string]string) (path, digest string) {
	t.Helper()
	img, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{
		OS:           "linux",
		Architecture: arch,
		Config:       v1.Config{Entrypoint: []string{"/hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	img, err = mutate.Append(img, mutate.Addendum{Layer: fileLayer(t, files)})
	if err != nil {
		t.Fatal(err)
	}
	attest, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{
		OS:           "unknown",
		Architecture: "unknown",
	})
	if err != nil {
		t.Fatal(err)
	}
	inner := mutate.AppendManifests(empty.Index,
		mutate.IndexAddendum{
			Add: img,
			Descriptor: v1.Descriptor{
				Platform: &v1.Platform{OS: "linux", Architecture: arch},
			},
		},
		mutate.IndexAddendum{
			Add: attest,
			Descriptor: v1.Descriptor{
				Platform: &v1.Platform{OS: "unknown", Architecture: "unknown"},
			},
		},
	)
	outer := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: inner})
	dir := t.TempDir()
	if _, err := layout.Write(dir, outer); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), "oci-nested.tar")
	tarDir(t, dir, path)
	sum, err := fileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, "sha256:" + sum
}

func tarDir(t *testing.T, dir, dest string) {
	t.Helper()
	f, err := os.Create(dest)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	tw := tar.NewWriter(f)
	err = filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil || p == dir {
			return err
		}
		rel, err := filepath.Rel(dir, p)
		if err != nil {
			return err
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		hdr, err := tar.FileInfoHeader(info, "")
		if err != nil {
			return err
		}
		hdr.Name = filepath.ToSlash(rel)
		if d.IsDir() {
			hdr.Name += "/"
		}
		if err := tw.WriteHeader(hdr); err != nil {
			return err
		}
		if d.IsDir() {
			return nil
		}
		src, err := os.Open(p)
		if err != nil {
			return err
		}
		defer src.Close()
		_, err = io.Copy(tw, src)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
}

// The OCI-layout image reads its blobs lazily off the staging dir, so dropping
// that dir before mutate.Extract runs breaks every layout archive.
func TestDownloadOCILayoutArchive(t *testing.T) {
	path, digest := writeOCILayoutArchive(t, runtime.GOARCH, map[string]string{"hello": "#!/bin/true\n"})
	mgr := NewManager(t.TempDir(), LocalFetcher{})
	ref := &pb.ArtifactRef{
		Type: pb.ArtifactType_ARTIFACT_TYPE_OCI_IMAGE, Name: "s", Version: "v1",
		Digest: digest, Uri: "file://" + path,
	}
	if err := mgr.Download(context.Background(), "s", ref, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(mgr.RootfsPath("s", "v1"), "hello")); err != nil {
		t.Fatalf("hello missing from rootfs: %v", err)
	}
	meta, err := mgr.readOCIMeta("s", "v1")
	if err != nil {
		t.Fatal(err)
	}
	if len(meta.Entrypoint) != 1 || meta.Entrypoint[0] != "/hello" {
		t.Fatalf("meta = %+v", meta)
	}
	// The staging dir lives next to image.tar and must not survive the unpack.
	entries, err := os.ReadDir(mgr.ReleaseDir("s", "v1"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "oci-layout-") {
			t.Fatalf("staging dir %s left behind", e.Name())
		}
	}
}

// writeBareDescriptorOCIArchive mirrors `podman save --format oci-archive`:
// the index descriptor carries no platform at all, so the only truth about
// the image's architecture is its config file.
func writeBareDescriptorOCIArchive(t *testing.T, arch string, files map[string]string) (path, digest string) {
	t.Helper()
	img, err := mutate.ConfigFile(empty.Image, &v1.ConfigFile{
		OS:           "linux",
		Architecture: arch,
		Config:       v1.Config{Entrypoint: []string{"/hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	img, err = mutate.Append(img, mutate.Addendum{Layer: fileLayer(t, files)})
	if err != nil {
		t.Fatal(err)
	}
	idx := mutate.AppendManifests(empty.Index, mutate.IndexAddendum{Add: img})
	dir := t.TempDir()
	if _, err := layout.Write(dir, idx); err != nil {
		t.Fatal(err)
	}
	path = filepath.Join(t.TempDir(), "oci-bare.tar")
	tarDir(t, dir, path)
	sum, err := fileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	return path, "sha256:" + sum
}

// A descriptor with no platform passes platformOK on every field, so the
// layout path must confirm against the config file the way the docker-archive
// path does. Otherwise a foreign-arch image unpacks cleanly and only fails at
// exec with "exec format error".
func TestLoadImageBareDescriptorChecksConfigPlatform(t *testing.T) {
	path, _ := writeBareDescriptorOCIArchive(t, otherArch(), map[string]string{"hello": "ok"})
	_, _, release, err := loadImage(context.Background(), path)
	if release != nil {
		defer release()
	}
	if err == nil {
		t.Fatal("accepted a foreign-arch image behind a platformless descriptor")
	}
	if !strings.Contains(err.Error(), otherArch()) {
		t.Fatalf("error should name the image arch it rejected: %v", err)
	}
}

// The same shape on the host arch must still load: the config confirms it.
func TestLoadImageBareDescriptorAcceptsHostArch(t *testing.T) {
	path, _ := writeBareDescriptorOCIArchive(t, runtime.GOARCH, map[string]string{"hello": "ok"})
	img, plat, release, err := loadImage(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if !strings.EqualFold(plat.Architecture, runtime.GOARCH) {
		t.Fatalf("platform = %s, want linux/%s", plat.String(), runtime.GOARCH)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Config.Entrypoint) != 1 || cfg.Config.Entrypoint[0] != "/hello" {
		t.Fatalf("entrypoint = %v", cfg.Config.Entrypoint)
	}
}

func TestLoadImageNestedOCILayout(t *testing.T) {
	path, _ := writeNestedOCILayoutArchive(t, runtime.GOARCH, map[string]string{"hello": "ok"})
	img, plat, release, err := loadImage(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if !strings.EqualFold(plat.OS, "linux") || !strings.EqualFold(plat.Architecture, runtime.GOARCH) {
		t.Fatalf("platform = %s, want linux/%s", plat.String(), runtime.GOARCH)
	}
	cfg, err := img.ConfigFile()
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Config.Entrypoint) != 1 || cfg.Config.Entrypoint[0] != "/hello" {
		t.Fatalf("entrypoint = %v", cfg.Config.Entrypoint)
	}
}

func TestDownloadNestedOCILayoutArchive(t *testing.T) {
	path, digest := writeNestedOCILayoutArchive(t, runtime.GOARCH, map[string]string{"hello": "#!/bin/true\n"})
	mgr := NewManager(t.TempDir(), LocalFetcher{})
	ref := &pb.ArtifactRef{
		Type: pb.ArtifactType_ARTIFACT_TYPE_OCI_IMAGE, Name: "s", Version: "v1",
		Digest: digest, Uri: "file://" + path,
	}
	if err := mgr.Download(context.Background(), "s", ref, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(mgr.RootfsPath("s", "v1"), "hello")); err != nil {
		t.Fatalf("hello missing from rootfs: %v", err)
	}
}

// An image may carry a symlink pointing anywhere (it is resolved inside the
// container after pivot_root), but extraction must never follow it on the host.
func TestExtractFlattenedSymlinkNoHostEscape(t *testing.T) {
	outside := t.TempDir()
	buf := &bytes.Buffer{}
	tw := tar.NewWriter(buf)
	write := func(hdr *tar.Header, body string) {
		hdr.Size = int64(len(body))
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	write(&tar.Header{Name: "esc", Typeflag: tar.TypeSymlink, Linkname: outside, Mode: 0o777}, "")
	write(&tar.Header{Name: "esc/evil", Typeflag: tar.TypeDir, Mode: 0o755}, "")
	write(&tar.Header{Name: "esc/evil/pwned", Typeflag: tar.TypeReg, Mode: 0o644}, "x")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	dest := filepath.Join(t.TempDir(), "rootfs")
	err := extractFlattened(context.Background(), bytes.NewReader(buf.Bytes()), dest)
	if err == nil {
		t.Fatal("expected extraction to refuse traversing the escaping symlink")
	}
	if _, err := os.Stat(filepath.Join(outside, "evil")); !os.IsNotExist(err) {
		t.Fatalf("escaped the destination: %v", err)
	}
}

// A release dir's mtime is bumped by any later write into it (a config
// re-fetch, LinkReleaseShared), so GC must rank by the recorded install time.
func TestGCReleasesRanksByStampNotMtime(t *testing.T) {
	mgr := NewManager(t.TempDir(), LocalFetcher{})
	mgr.ReleaseRetention = 3
	now := time.Now()
	for i, v := range []string{"v1", "v2", "v3", "v4"} {
		dir := mgr.ReleaseDir("s", v)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		stamp := now.Add(time.Duration(i) * time.Hour)
		if err := os.WriteFile(ReleaseStampPath(dir),
			[]byte(strconv.FormatInt(stamp.UnixNano(), 10)), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	// v1 is the oldest install but now has the newest mtime.
	touched := now.Add(24 * time.Hour)
	if err := os.Chtimes(mgr.ReleaseDir("s", "v1"), touched, touched); err != nil {
		t.Fatal(err)
	}
	if err := mgr.GCReleases("s", []string{"v4", "v3"}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(mgr.ReleaseDir("s", "v1")); !os.IsNotExist(err) {
		t.Fatal("v1 is the oldest install and should have been gc'd despite its mtime")
	}
	if _, err := os.Stat(mgr.ReleaseDir("s", "v2")); err != nil {
		t.Fatalf("v2 is the newest extra and should survive: %v", err)
	}
}

// The estimate must not read the layers: mutate.Extract decompresses them
// again immediately afterwards.
func TestEstimateUnpackBytesDoesNotReadLayers(t *testing.T) {
	path, _ := writeDockerArchive(t, runtime.GOARCH, map[string]string{"hello": "hi"})
	img, _, release, err := loadImage(context.Background(), path)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	layers, err := img.Layers()
	if err != nil {
		t.Fatal(err)
	}
	// This archive's layers are gzipped, so the estimate is 3x the stored size
	// — not the exact uncompressed total a full decompression pass would give.
	var stored, plain int64
	for _, l := range layers {
		n, err := l.Size()
		if err != nil {
			t.Fatal(err)
		}
		stored += n
		mt, err := l.MediaType()
		if err != nil {
			t.Fatal(err)
		}
		if !compressedLayer(mt) {
			t.Skipf("layer media type %s is not compressed", mt)
		}
		r, err := l.Uncompressed()
		if err != nil {
			t.Fatal(err)
		}
		n, err = io.Copy(io.Discard, r)
		r.Close()
		if err != nil {
			t.Fatal(err)
		}
		plain += n
	}
	got, err := estimateUnpackBytes(img)
	if err != nil {
		t.Fatal(err)
	}
	if want := stored*3 + 64<<20; got != want {
		t.Fatalf("estimate = %d, want %d", got, want)
	}
	if got == plain+64<<20 {
		t.Fatal("estimate matches the full-decompression total: the pre-pass is back")
	}
}
