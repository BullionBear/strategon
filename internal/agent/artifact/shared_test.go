package artifact

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

func TestEnsureSwitchSharedAndRollback(t *testing.T) {
	src := t.TempDir()
	base := t.TempDir()
	mgr := NewManager(base, LocalFetcher{})
	ctx := context.Background()

	p1, d1 := writeSource(t, src, "instruments-v1.json", `{"v":1}`)
	p2, d2 := writeSource(t, src, "instruments-v2.json", `{"v":2}`)
	ref1 := &pb.ArtifactRef{Name: "instruments.json", Version: "v1", Digest: d1, Uri: "file://" + p1}
	ref2 := &pb.ArtifactRef{Name: "instruments.json", Version: "v2", Digest: d2, Uri: "file://" + p2}

	if err := mgr.EnsureSharedFile(ctx, "instruments.json", ref1); err != nil {
		t.Fatalf("ensure v1: %v", err)
	}
	if err := mgr.SwitchSharedTo("instruments.json", d1); err != nil {
		t.Fatalf("switch v1: %v", err)
	}
	if got := mgr.RunningSharedDigest("instruments.json"); got != d1 {
		t.Fatalf("running = %q, want %q", got, d1)
	}
	data, err := os.ReadFile(mgr.SharedLink("instruments.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"v":1}` {
		t.Fatalf("content = %q", data)
	}

	if err := mgr.EnsureSharedFile(ctx, "instruments.json", ref2); err != nil {
		t.Fatalf("ensure v2: %v", err)
	}
	if err := mgr.SwitchSharedTo("instruments.json", d2); err != nil {
		t.Fatalf("switch v2: %v", err)
	}
	if got := mgr.RunningSharedDigest("instruments.json"); got != d2 {
		t.Fatalf("running after v2 = %q", got)
	}

	// O(1) rollback: re-point without re-fetch.
	if err := mgr.SwitchSharedTo("instruments.json", d1); err != nil {
		t.Fatalf("rollback: %v", err)
	}
	data, err = os.ReadFile(mgr.SharedLink("instruments.json"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != `{"v":1}` {
		t.Fatalf("after rollback content = %q", data)
	}
}

func TestEnsureSharedRejectsBadDigest(t *testing.T) {
	src := t.TempDir()
	base := t.TempDir()
	mgr := NewManager(base, LocalFetcher{})
	p, _ := writeSource(t, src, "x.json", `ok`)
	bad := &pb.ArtifactRef{Name: "x.json", Version: "v1", Digest: "sha256:deadbeef", Uri: "file://" + p}
	if err := mgr.EnsureSharedFile(context.Background(), "x.json", bad); err == nil {
		t.Fatal("expected digest mismatch")
	}
	if _, err := os.Stat(mgr.SharedStorePath(bad.Digest, "x.json")); !os.IsNotExist(err) {
		t.Fatalf("bad store entry should be removed, stat err=%v", err)
	}
}

func TestLinkReleaseSharedAndDownload(t *testing.T) {
	src := t.TempDir()
	base := t.TempDir()
	mgr := NewManager(base, LocalFetcher{})
	p, d := writeSource(t, src, "bin", "binary")
	art := &pb.ArtifactRef{Version: "v1", Digest: d, Uri: "file://" + p}
	if err := mgr.Download(context.Background(), "s", art, nil); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(mgr.ReleaseDir("s", "v1"), "shared")
	dst, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("release shared symlink: %v", err)
	}
	if dst != filepath.Join("..", "..", "..", "shared") {
		t.Fatalf("symlink target = %q", dst)
	}
	// Resolves to machine shared root.
	resolved, err := filepath.EvalSymlinks(link)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := filepath.EvalSymlinks(mgr.SharedRoot())
	if resolved != want {
		t.Fatalf("resolved = %q, want %q", resolved, want)
	}
}

func TestGCSharedRetainsLiveAndN(t *testing.T) {
	src := t.TempDir()
	base := t.TempDir()
	mgr := NewManager(base, LocalFetcher{})
	ctx := context.Background()
	name := "instruments.json"
	var digests []string
	for i, body := range []string{`a`, `b`, `c`, `d`} {
		p, d := writeSource(t, src, body+".json", body)
		ref := &pb.ArtifactRef{Name: name, Version: "v", Digest: d, Uri: "file://" + p}
		if err := mgr.EnsureSharedFile(ctx, name, ref); err != nil {
			t.Fatalf("ensure %d: %v", i, err)
		}
		digests = append(digests, d)
		if err := mgr.SwitchSharedTo(name, d); err != nil {
			t.Fatalf("switch %d: %v", i, err)
		}
	}
	if err := mgr.GCShared(2, map[string]struct{}{name: {}}); err != nil {
		t.Fatal(err)
	}
	// Live (last) must remain.
	if _, err := os.Stat(mgr.SharedStorePath(digests[3], name)); err != nil {
		t.Fatalf("live store entry missing: %v", err)
	}
	// Oldest should be gone with retention 2.
	if _, err := os.Stat(mgr.SharedStorePath(digests[0], name)); !os.IsNotExist(err) {
		t.Fatalf("oldest entry should be GC'd, err=%v", err)
	}
}
