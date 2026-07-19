package artifact

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

func writeSource(t *testing.T, dir, name, content string) (path, digest string) {
	t.Helper()
	path = filepath.Join(dir, name)
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(content))
	return path, "sha256:" + hex.EncodeToString(sum[:])
}

func TestDownloadVerifySwitchAndO1Rollback(t *testing.T) {
	src := t.TempDir()
	base := t.TempDir()
	mgr := NewManager(base, LocalFetcher{})
	ctx := context.Background()

	p1, d1 := writeSource(t, src, "v1bin", "binary-one")
	p2, d2 := writeSource(t, src, "v2bin", "binary-two")
	v1 := &pb.ArtifactRef{Version: "v1", Digest: d1, Uri: "file://" + p1}
	v2 := &pb.ArtifactRef{Version: "v2", Digest: d2, Uri: "file://" + p2}

	// Deploy v1.
	if err := mgr.Download(ctx, "s", v1, nil); err != nil {
		t.Fatalf("download v1: %v", err)
	}
	if err := mgr.Verify("s", v1); err != nil {
		t.Fatalf("verify v1: %v", err)
	}
	if err := mgr.SwitchTo("s", "v1"); err != nil {
		t.Fatalf("switch v1: %v", err)
	}
	if got := mgr.CurrentVersion("s"); got != "v1" {
		t.Fatalf("current = %q, want v1", got)
	}

	// Deploy v2.
	if err := mgr.Download(ctx, "s", v2, nil); err != nil {
		t.Fatalf("download v2: %v", err)
	}
	if err := mgr.Verify("s", v2); err != nil {
		t.Fatalf("verify v2: %v", err)
	}
	if err := mgr.SwitchTo("s", "v2"); err != nil {
		t.Fatalf("switch v2: %v", err)
	}
	if got := mgr.CurrentVersion("s"); got != "v2" {
		t.Fatalf("current = %q, want v2", got)
	}

	// O(1) rollback: v1 is still present, no re-download needed.
	if !mgr.HasRelease("s", "v1") {
		t.Fatalf("v1 release should still be present for O(1) rollback")
	}
	if err := mgr.SwitchTo("s", "v1"); err != nil {
		t.Fatalf("rollback switch: %v", err)
	}
	if got := mgr.CurrentVersion("s"); got != "v1" {
		t.Fatalf("after rollback current = %q, want v1", got)
	}

	// The current binary resolves through the symlink.
	data, err := os.ReadFile(mgr.CurrentBinaryPath("s"))
	if err != nil {
		t.Fatalf("read current bin: %v", err)
	}
	if string(data) != "binary-one" {
		t.Fatalf("current bin content = %q, want binary-one", data)
	}
}

func TestConfigPreservesURIExtension(t *testing.T) {
	src := t.TempDir()
	base := t.TempDir()
	mgr := NewManager(base, LocalFetcher{})
	pBin, dBin := writeSource(t, src, "bin", "binary")
	pCfg, dCfg := writeSource(t, src, "settings.yml", "foo: bar")
	art := &pb.ArtifactRef{Version: "v1", Digest: dBin, Uri: "file://" + pBin}
	cfg := &pb.ArtifactRef{Version: "c1", Digest: dCfg, Uri: "file://" + pCfg}

	if err := mgr.Download(context.Background(), "s", art, cfg); err != nil {
		t.Fatal(err)
	}
	got := mgr.ConfigPath("s", "v1", cfg)
	if filepath.Base(got) != "config.yml" {
		t.Fatalf("config path = %q, want …/config.yml", got)
	}
	data, err := os.ReadFile(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "foo: bar" {
		t.Fatalf("config content = %q", data)
	}
	if ConfigFileName(nil) != "config" || ConfigFileName(&pb.ArtifactRef{Uri: "file:///x/config"}) != "config" {
		t.Fatalf("no-ext basename wrong")
	}
}

func TestVerifyRejectsBadDigest(t *testing.T) {
	src := t.TempDir()
	base := t.TempDir()
	mgr := NewManager(base, LocalFetcher{})
	p1, _ := writeSource(t, src, "v1bin", "binary-one")
	bad := &pb.ArtifactRef{Version: "v1", Digest: "sha256:deadbeef", Uri: "file://" + p1}
	if err := mgr.Download(context.Background(), "s", bad, nil); err != nil {
		t.Fatalf("download: %v", err)
	}
	if err := mgr.Verify("s", bad); err == nil {
		t.Fatalf("verify must reject a mismatched digest")
	}
}
