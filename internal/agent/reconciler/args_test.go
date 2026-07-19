package reconciler

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/agent/artifact"
)

func TestExpandPlaceholders(t *testing.T) {
	vals := map[string]string{
		"CONFIG":      "/opt/s/current/config.yml",
		"RELEASE_DIR": "/opt/s/releases/v1",
		"BINARY":      "/opt/s/current/bin",
	}
	got, err := expandPlaceholders("-c ${CONFIG}", vals)
	if err != nil {
		t.Fatal(err)
	}
	if got != "-c /opt/s/current/config.yml" {
		t.Fatalf("got %q", got)
	}
	got, err = expandPlaceholders("${RELEASE_DIR}/x ${BINARY}", vals)
	if err != nil {
		t.Fatal(err)
	}
	if got != "/opt/s/releases/v1/x /opt/s/current/bin" {
		t.Fatalf("got %q", got)
	}
	if _, err := expandPlaceholders("${TYPO}", vals); err == nil {
		t.Fatal("expected unknown placeholder error")
	}
}

func TestRenderArgsViaCurrentSymlink(t *testing.T) {
	src := t.TempDir()
	base := t.TempDir()
	mgr := artifact.NewManager(base, artifact.LocalFetcher{})

	binPath := filepath.Join(src, "bin")
	cfgPath := filepath.Join(src, "app.yml")
	if err := os.WriteFile(binPath, []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("k: v\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	art := &pb.ArtifactRef{Version: "v42", Digest: "sha256:x", Uri: "file://" + binPath}
	cfg := &pb.ArtifactRef{Version: "c17", Digest: "sha256:y", Uri: "file://" + cfgPath}
	if err := mgr.Download(context.Background(), "s", art, cfg); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SwitchTo("s", "v42"); err != nil {
		t.Fatal(err)
	}

	r := &Reconciler{deps: Deps{Artifacts: mgr}}
	spec := &pb.StrategyAssignmentSpec{
		Strategy: "s",
		Artifact: art,
		Config:   cfg,
		Args:     []string{"-c", "${CONFIG}", "--dir", "${RELEASE_DIR}"},
	}
	args, err := r.renderArgs(spec)
	if err != nil {
		t.Fatal(err)
	}
	wantCfg, _ := filepath.Abs(mgr.CurrentConfigPath("s", cfg))
	wantRel, err := mgr.CurrentReleaseDir("s")
	if err != nil {
		t.Fatal(err)
	}
	if args[0] != "-c" || args[1] != wantCfg {
		t.Fatalf("args = %#v, want -c %s", args, wantCfg)
	}
	if args[2] != "--dir" || args[3] != wantRel {
		t.Fatalf("args = %#v, want --dir %s", args, wantRel)
	}
	if filepath.Base(args[1]) != "config.yml" {
		t.Fatalf("config basename = %q, want config.yml", filepath.Base(args[1]))
	}
}
