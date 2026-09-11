package reconciler

import (
	"context"
	"os"
	"path/filepath"
	"strings"
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
func TestExpandVolumePlaceholderEXECAndOCI(t *testing.T) {
	r, _, mgr, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	seedRelease(t, mgr, "s", "v1")
	if err := mgr.SwitchTo("s", "v1"); err != nil {
		t.Fatal(err)
	}
	if _, err := mgr.EnsureVolumeDir("nats-a-data"); err != nil {
		t.Fatal(err)
	}
	spec := &pb.StrategyAssignmentSpec{
		Strategy: "s",
		VolumeMounts: []*pb.VolumeMount{
			{Name: "nats-a-data", ContainerPath: "/var/lib/mftik"},
		},
		Args: []string{"--data", "${VOLUME:nats-a-data}"},
		Env:  map[string]string{"MFTIK_DATA": "${VOLUME:nats-a-data}", "KEEP": "${CONFIG}"},
	}
	execLaunch := &pb.ArtifactRef{Type: pb.ArtifactType_ARTIFACT_TYPE_BINARY, Version: "v1"}
	args, err := r.renderArgs(spec, execLaunch)
	if err != nil {
		t.Fatal(err)
	}
	wantHost, _ := filepath.Abs(mgr.VolumeDir("nats-a-data"))
	if len(args) != 2 || args[1] != wantHost {
		t.Fatalf("exec args = %#v, want host %s", args, wantHost)
	}
	env, err := r.renderEnv(spec, execLaunch, envPairs(spec.GetEnv()))
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]string{}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		got[k] = v
	}
	if got["MFTIK_DATA"] != wantHost {
		t.Fatalf("exec env = %#v", got)
	}
	if got["KEEP"] != "${CONFIG}" {
		t.Fatalf("${CONFIG} in env must stay verbatim: %#v", got)
	}

	ociLaunch := &pb.ArtifactRef{Type: pb.ArtifactType_ARTIFACT_TYPE_OCI_IMAGE, Version: "v2"}
	args, err = r.renderArgs(spec, ociLaunch)
	if err != nil {
		t.Fatal(err)
	}
	if args[1] != "/var/lib/mftik" {
		t.Fatalf("oci args = %#v", args)
	}
	env, err = r.renderEnv(spec, ociLaunch, envPairs(spec.GetEnv()))
	if err != nil {
		t.Fatal(err)
	}
	got = map[string]string{}
	for _, e := range env {
		k, v, _ := strings.Cut(e, "=")
		got[k] = v
	}
	if got["MFTIK_DATA"] != "/var/lib/mftik" {
		t.Fatalf("oci env = %#v", got)
	}
}

func TestExpandVolumeUnknownName(t *testing.T) {
	r := &Reconciler{}
	spec := &pb.StrategyAssignmentSpec{}
	if _, err := r.expandVolumePlaceholders("${VOLUME:nope}", spec, &pb.ArtifactRef{}); err == nil {
		t.Fatal("expected unknown volume")
	}
}

func TestBuildStartSpecOCIVolumeBinds(t *testing.T) {
	r, _, mgr, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	if _, err := mgr.EnsureVolumeDir("data"); err != nil {
		t.Fatal(err)
	}
	seedRelease(t, mgr, "s", "v2")
	rootfs := mgr.RootfsPath("s", "v2")
	if err := os.MkdirAll(rootfs, 0o755); err != nil {
		t.Fatal(err)
	}
	meta := []byte(`{"schema_version":1,"entrypoint":["/bin/true"]}`)
	if err := os.WriteFile(mgr.OCIMetaPath("s", "v2"), meta, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SwitchTo("s", "v2"); err != nil {
		t.Fatal(err)
	}
	launch := &pb.ArtifactRef{Type: pb.ArtifactType_ARTIFACT_TYPE_OCI_IMAGE, Version: "v2"}
	spec := &pb.StrategyAssignmentSpec{
		Strategy:     "s",
		Artifact:     launch,
		VolumeMounts: []*pb.VolumeMount{{Name: "data", ContainerPath: "/var/lib/mftik"}},
		Env:          map[string]string{"D": "${VOLUME:data}"},
	}
	sp, err := r.buildStartSpec(spec, launch)
	if err != nil {
		t.Fatal(err)
	}
	if sp.Driver != driver.KindOCI {
		t.Fatalf("driver = %v", sp.Driver)
	}
	if len(sp.VolumeBinds) != 1 || sp.VolumeBinds[0].Container != "/var/lib/mftik" {
		t.Fatalf("binds = %+v", sp.VolumeBinds)
	}
	found := false
	for _, e := range sp.Env {
		if e == "D=/var/lib/mftik" {
			found = true
		}
	}
	if !found {
		t.Fatalf("env = %#v", sp.Env)
	}

	binLaunch := artRef("v1", "sha256:v1")
	seedRelease(t, mgr, "s", "v1")
	if err := mgr.SwitchTo("s", "v1"); err != nil {
		t.Fatal(err)
	}
	sp, err = r.buildStartSpec(spec, binLaunch)
	if err != nil {
		t.Fatal(err)
	}
	if sp.Driver != driver.KindExec {
		t.Fatalf("rollback BINARY driver = %v", sp.Driver)
	}
	if len(sp.VolumeBinds) != 0 {
		t.Fatalf("EXEC must ignore VolumeBinds: %+v", sp.VolumeBinds)
	}
	wantHost, _ := filepath.Abs(mgr.VolumeDir("data"))
	found = false
	for _, e := range sp.Env {
		if e == "D="+wantHost {
			found = true
		}
	}
	if !found {
		t.Fatalf("exec rollback env = %#v want D=%s", sp.Env, wantHost)
	}
}

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
