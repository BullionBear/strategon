package reconciler

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/agent/artifact"
	"github.com/bullionbear/strategon/internal/agent/driver"
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
	args, err := r.renderArgs(spec, spec.Artifact)
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

func TestRenderArgsOCIRejectsReleaseDirAndBinary(t *testing.T) {
	r := &Reconciler{}
	launch := &pb.ArtifactRef{Type: pb.ArtifactType_ARTIFACT_TYPE_OCI_IMAGE}
	spec := &pb.StrategyAssignmentSpec{
		Strategy: "s",
		Artifact: launch,
		Args:     []string{"--dir", "${RELEASE_DIR}"},
	}
	if _, err := r.renderArgs(spec, launch); err == nil {
		t.Fatal("expected ${RELEASE_DIR} rejected for OCI")
	}
	spec.Args = []string{"${BINARY}"}
	if _, err := r.renderArgs(spec, launch); err == nil {
		t.Fatal("expected ${BINARY} rejected for OCI")
	}
}

func TestBuildStartSpecUsesLaunchTypeNotSpecDriver(t *testing.T) {
	r, _, mgr, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	seedRelease(t, mgr, "s", "v1")
	if err := mgr.SwitchTo("s", "v1"); err != nil {
		t.Fatal(err)
	}
	desired := &pb.StrategyAssignmentSpec{
		Strategy: "s",
		Artifact: &pb.ArtifactRef{Type: pb.ArtifactType_ARTIFACT_TYPE_OCI_IMAGE, Version: "v2"},
		Driver:   pb.ExecutionDriver_EXECUTION_DRIVER_OCI,
	}
	launch := artRef("v1", "sha256:v1")
	sp, err := r.buildStartSpec(desired, launch)
	if err != nil {
		t.Fatal(err)
	}
	if sp.Driver != driver.KindExec {
		t.Fatalf("driver = %v, want EXEC from launch type", sp.Driver)
	}
}

// A nil Env means "inherit the parent's environment" to exec.Cmd, which would
// hand the agent's own env (control-plane URL, object-store credentials) to the
// strategy container. mergeEnv must return an empty slice instead.
func TestMergeEnvNeverNil(t *testing.T) {
	got := mergeEnv(nil, nil)
	if got == nil {
		t.Fatal("mergeEnv(nil, nil) = nil; the container would inherit the agent env")
	}
	if len(got) != 0 {
		t.Fatalf("mergeEnv(nil, nil) = %#v, want empty", got)
	}
	if got := mergeEnv(nil, map[string]string{}); got == nil {
		t.Fatal("mergeEnv with an empty spec env = nil")
	}
	// Overlay still works: spec wins over the image value.
	merged := mergeEnv([]string{"A=1", "B=2"}, map[string]string{"B": "3"})
	if len(merged) != 2 {
		t.Fatalf("merged = %#v", merged)
	}
	for _, e := range merged {
		if e == "B=2" {
			t.Fatalf("spec env did not override image env: %#v", merged)
		}
	}
}
