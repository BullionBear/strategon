//go:build linux

// TestDeployWithConfigArgsEnvAndScheduleEndToEnd exercises the config/binary
// separation feature through the real control-plane human
// API (RegisterArtifact, SetDeployment, SetSchedule) down to a real forked
// process: it proves that a strategy binary and its config are registered as
// independent artifacts, that ${CONFIG}/${RELEASE_DIR}/${BINARY} placeholders
// in args are rendered against the on-disk release the agent actually
// downloaded, that env vars reach the process, and that a cron schedule
// attached to the same deployment round-trips through the machine view.
package integration

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/gen/strategyplatform/v1/strategyplatformv1connect"
	"github.com/bullionbear/strategon/internal/agent/artifact"
	"github.com/bullionbear/strategon/internal/agent/driver"
	"github.com/bullionbear/strategon/internal/agent/health"
	"github.com/bullionbear/strategon/internal/agent/reconciler"
	"github.com/bullionbear/strategon/internal/agent/stream"
	"github.com/bullionbear/strategon/internal/clock"
	"github.com/bullionbear/strategon/internal/controlplane/api"
	"github.com/bullionbear/strategon/internal/controlplane/grpcstream"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func TestDeployWithConfigArgsEnvAndScheduleEndToEnd(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh required")
	}

	// --- Control plane: AgentService (agent-facing) + ControlPlaneService
	// (human-facing) sharing one store, wired exactly like cmd/controlplane. ---
	hub := store.NewHub()
	st := store.NewMemory(hub)
	agentSrv := grpcstream.New(st, grpcstream.WithResync(500*time.Millisecond))
	humanSrv := api.New(st, hub, agentSrv, nil)

	mux := http.NewServeMux()
	agentPath, agentHandler := strategyplatformv1connect.NewAgentServiceHandler(agentSrv)
	mux.Handle(agentPath, agentHandler)
	humanPath, humanHandler := strategyplatformv1connect.NewControlPlaneServiceHandler(humanSrv)
	mux.Handle(humanPath, humanHandler)
	ts := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	ts.Start()
	defer ts.Close()

	humanClient := strategyplatformv1connect.NewControlPlaneServiceClient(http.DefaultClient, ts.URL)

	// --- Agent ---
	base := t.TempDir()
	out := make(chan *pb.AgentMessage, 256)
	rec := reconciler.New(reconciler.Deps{
		Driver:       driver.NewExecDriver(""),
		Artifacts:    artifact.NewManager(base, artifact.LocalFetcher{}),
		Health:       health.AlwaysReady{},
		Clock:        clock.Real{},
		Out:          out,
		TickInterval: 100 * time.Millisecond,
	})
	httpClient := &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			var d net.Dialer
			return d.DialContext(ctx, network, addr)
		},
	}}
	agentClient := &stream.Client{
		Register:    &pb.Register{MachineId: "m1", Hostname: "test", AgentVersion: 1},
		Client:      strategyplatformv1connect.NewAgentServiceClient(httpClient, ts.URL, connect.WithGRPC()),
		Out:         out,
		Submit:      rec.SubmitDesired,
		ObservedGen: rec.ObservedGeneration,
		Clock:       clock.Real{},
		Heartbeat:   200 * time.Millisecond,
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go rec.Run(ctx)
	go agentClient.Run(ctx)

	waitFor(t, 5*time.Second, func() bool {
		_, ok := st.GetMachine("m1")
		return ok
	}, "agent to register")

	// --- Binary that dumps its resolved argv + env, then stays alive. ---
	srcDir := t.TempDir()
	outFile := filepath.Join(t.TempDir(), "dump.txt")
	script := "#!/bin/sh\n" +
		"{\n\tfor a in \"$@\"; do printf '%s\\n' \"$a\"; done\n\tprintf 'FOO=%s\\n' \"$FOO\"\n} > \"$OUT_FILE\"\n" +
		"exec sleep 300\n"
	binPath := filepath.Join(srcDir, "strategy.sh")
	if err := os.WriteFile(binPath, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	binDigest := sha256Hex(t, binPath)

	cfgContent := "mode: aggressive\nthreshold: 0.5\n"
	cfgPath := filepath.Join(srcDir, "strategy.yml")
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgDigest := sha256Hex(t, cfgPath)

	reqCtx := context.Background()

	// --- Register binary + config as independent artifacts. ---
	if _, err := humanClient.RegisterArtifact(reqCtx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "s", Version: "v1", Digest: "sha256:" + binDigest, Uri: "file://" + binPath},
	})); err != nil {
		t.Fatal(err)
	}
	cfgRef := &pb.ArtifactRef{Name: "s-config", Version: "c1", Digest: "sha256:" + cfgDigest, Uri: "file://" + cfgPath}
	if _, err := humanClient.RegisterArtifact(reqCtx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: cfgRef,
	})); err != nil {
		t.Fatal(err)
	}

	// --- SetDeployment: binary + config + placeholder args + env, in one call. ---
	// Create-then-start: lands halted until an explicit Start.
	setResp, err := humanClient.SetDeployment(reqCtx, connect.NewRequest(&pb.SetDeploymentRequest{
		MachineId:       "m1",
		Strategy:        "s",
		ArtifactVersion: "v1",
		ConfigVersion:   "c1",
		Args:            []string{"-c", "${CONFIG}", "--dir", "${RELEASE_DIR}", "--bin", "${BINARY}"},
		Env:             map[string]string{"FOO": "bar-baz", "OUT_FILE": outFile},
	}))
	if err != nil {
		t.Fatal(err)
	}
	if setResp.Msg.GetGeneration() != 1 {
		t.Fatalf("generation = %d, want 1", setResp.Msg.GetGeneration())
	}
	startResp, err := humanClient.Start(reqCtx, connect.NewRequest(&pb.StartRequest{
		MachineId: "m1", Strategy: "s",
	}))
	if err != nil {
		t.Fatal(err)
	}
	if startResp.Msg.GetGeneration() != 2 {
		t.Fatalf("Start generation = %d, want 2", startResp.Msg.GetGeneration())
	}

	// --- Attach a cron schedule to the same deployment. ---
	if _, err := humanClient.SetSchedule(reqCtx, connect.NewRequest(&pb.SetScheduleRequest{
		MachineId: "m1",
		Strategy:  "s",
		Schedules: []*pb.CronSchedule{{
			Name:     "hourly-restart",
			CronExpr: "0 * * * *",
			Timezone: "UTC",
			Action:   pb.CronAction_CRON_ACTION_RESTART,
		}},
	})); err != nil {
		t.Fatal(err)
	}

	// --- Assert convergence to HEALTHY v1/c1. ---
	var pid int32
	waitFor(t, 15*time.Second, func() bool {
		mrec, ok := st.GetMachine("m1")
		if !ok {
			return false
		}
		s := mrec.Status["s"]
		if s == nil || s.GetPhase() != pb.DeployPhase_DEPLOY_PHASE_HEALTHY {
			return false
		}
		if s.GetRunningArtifact().GetVersion() != "v1" || s.GetRunningConfig().GetVersion() != "c1" {
			return false
		}
		pid = s.GetPid()
		return pid > 0
	}, "strategy to converge to HEALTHY v1/c1")

	if !processAlive(int(pid)) {
		t.Fatalf("strategy process %d should be alive", pid)
	}

	// --- The config artifact landed on disk with its original extension. ---
	mgr := artifact.NewManager(base, artifact.LocalFetcher{})
	wantCfg, err := filepath.Abs(mgr.CurrentConfigPath("s", cfgRef))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(wantCfg) != "config.yml" {
		t.Fatalf("config basename = %q, want config.yml", filepath.Base(wantCfg))
	}
	gotCfg, err := os.ReadFile(wantCfg)
	if err != nil {
		t.Fatal(err)
	}
	if string(gotCfg) != cfgContent {
		t.Fatalf("deployed config content = %q, want %q", gotCfg, cfgContent)
	}

	// --- The process actually received the rendered args + env. ---
	waitForFile(t, 5*time.Second, outFile)
	dump, err := os.ReadFile(outFile)
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimRight(string(dump), "\n"), "\n")
	wantRelDir, err := mgr.CurrentReleaseDir("s")
	if err != nil {
		t.Fatal(err)
	}
	wantBin, err := filepath.Abs(mgr.CurrentBinaryPath("s"))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-c", wantCfg, "--dir", wantRelDir, "--bin", wantBin, "FOO=bar-baz"}
	if len(lines) != len(want) {
		t.Fatalf("dumped argv+env = %#v, want %#v", lines, want)
	}
	for i, w := range want {
		if lines[i] != w {
			t.Fatalf("dump[%d] = %q, want %q (full dump: %#v)", i, lines[i], w, lines)
		}
	}

	// --- The cron schedule round-trips into the machine view. ---
	m, err := humanClient.GetMachine(reqCtx, connect.NewRequest(&pb.GetMachineRequest{MachineId: "m1"}))
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, sv := range m.Msg.GetStrategies() {
		if sv.GetStrategy() != "s" {
			continue
		}
		for _, sched := range sv.GetSchedules() {
			if sched.GetName() == "hourly-restart" && sched.GetAction() == pb.CronAction_CRON_ACTION_RESTART {
				found = true
			}
		}
	}
	if !found {
		t.Fatalf("expected schedule in machine view, got %#v", m.Msg.GetStrategies())
	}

	// --- A version-only Deploy (no config/args/env in the request) must
	// preserve them, per the same rule verified at the API layer. Convergence
	// is digest-based, so v2 needs distinct content. ---
	binPathV2 := filepath.Join(srcDir, "strategy-v2.sh")
	if err := os.WriteFile(binPathV2, []byte(script+"# v2\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	binDigestV2 := sha256Hex(t, binPathV2)
	if _, err := humanClient.RegisterArtifact(reqCtx, connect.NewRequest(&pb.RegisterArtifactRequest{
		Artifact: &pb.ArtifactRef{Name: "s", Version: "v2", Digest: "sha256:" + binDigestV2, Uri: "file://" + binPathV2},
	})); err != nil {
		t.Fatal(err)
	}
	if _, err := humanClient.Deploy(reqCtx, connect.NewRequest(&pb.DeployRequest{
		MachineId: "m1", Strategy: "s", ArtifactVersion: "v2",
	})); err != nil {
		t.Fatal(err)
	}
	waitFor(t, 15*time.Second, func() bool {
		mrec, ok := st.GetMachine("m1")
		if !ok {
			return false
		}
		s := mrec.Status["s"]
		return s != nil && s.GetPhase() == pb.DeployPhase_DEPLOY_PHASE_HEALTHY && s.GetRunningArtifact().GetVersion() == "v2"
	}, "strategy to converge to HEALTHY v2")

	m2, ok := st.GetMachine("m1")
	if !ok {
		t.Fatal("machine missing")
	}
	spec := m2.Assignments["s"]
	if spec.GetConfig().GetVersion() != "c1" {
		t.Fatalf("Deploy should preserve config, got %q", spec.GetConfig().GetVersion())
	}
	if len(spec.GetArgs()) != 6 || spec.GetArgs()[1] != "${CONFIG}" {
		t.Fatalf("Deploy should preserve args, got %#v", spec.GetArgs())
	}
	if spec.GetEnv()["FOO"] != "bar-baz" {
		t.Fatalf("Deploy should preserve env, got %#v", spec.GetEnv())
	}
}

func sha256Hex(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

func waitForFile(t *testing.T, timeout time.Duration, path string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if info, err := os.Stat(path); err == nil && info.Size() > 0 {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", path)
}
