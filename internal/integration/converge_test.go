//go:build linux

// Package integration exercises the whole foundation in-process: a control
// plane (store + AgentService stream over h2c) and an agent (reconciler + exec
// driver + stream client). It proves the core thesis: change desired state ->
// the agent converges; remove it -> the agent retires the process.
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
	"strconv"
	"syscall"
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
	"github.com/bullionbear/strategon/internal/controlplane/grpcstream"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
)

func TestChangeDesiredConvergesThenRetires(t *testing.T) {
	if _, err := os.Stat("/bin/sh"); err != nil {
		t.Skip("/bin/sh required")
	}

	// --- Control plane over h2c ---
	st := store.NewMemory()
	srv := grpcstream.New(st, grpcstream.WithResync(500*time.Millisecond))
	mux := http.NewServeMux()
	path, handler := strategyplatformv1connect.NewAgentServiceHandler(srv)
	mux.Handle(path, handler)
	ts := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	ts.Start()
	defer ts.Close()

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
		AllowHTTP:      true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) { var d net.Dialer; return d.DialContext(ctx, network, addr) },
	}}
	client := &stream.Client{
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
	go client.Run(ctx)

	// Wait for the agent to register.
	waitFor(t, 5*time.Second, func() bool {
		_, ok := st.GetMachine("m1")
		return ok
	}, "agent to register")

	// --- Publish a strategy (change desired state) ---
	script := "#!/bin/sh\nexec sleep 300\n"
	src := filepath.Join(t.TempDir(), "strat.sh")
	if err := os.WriteFile(src, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(script))
	digest := "sha256:" + hex.EncodeToString(sum[:])

	spec := &pb.StrategyAssignmentSpec{
		Strategy: "s",
		Artifact: &pb.ArtifactRef{Type: pb.ArtifactType_ARTIFACT_TYPE_BINARY, Version: "v1", Digest: digest, Uri: "file://" + src},
		DeployPolicy: &pb.DeployPolicy{Startsecs: 1, HealthWindowSeconds: 5, MaxCrashesInWindow: 3, StopGraceSeconds: 3, EnableAutoRollback: true},
	}
	gen, err := st.SetAssignment("m1", "s", spec)
	if err != nil {
		t.Fatal(err)
	}
	srv.Notify("m1")

	// --- Assert convergence: HEALTHY, running v1, pid alive, symlink correct ---
	var pid int32
	waitFor(t, 15*time.Second, func() bool {
		rec, ok := st.GetMachine("m1")
		if !ok {
			return false
		}
		s := rec.Status["s"]
		if s == nil || s.GetPhase() != pb.DeployPhase_DEPLOY_PHASE_HEALTHY {
			return false
		}
		if s.GetRunningArtifact().GetVersion() != "v1" {
			return false
		}
		pid = s.GetPid()
		return pid > 0
	}, "strategy to converge to HEALTHY v1")

	mgr := artifact.NewManager(base, artifact.LocalFetcher{})
	if got := mgr.CurrentVersion("s"); got != "v1" {
		t.Fatalf("current symlink = %q, want v1", got)
	}
	if !processAlive(int(pid)) {
		t.Fatalf("strategy process %d should be alive", pid)
	}

	rec2, _ := st.GetMachine("m1")
	if rec2.ObservedGen < gen {
		t.Fatalf("observed generation %d should reach spec generation %d", rec2.ObservedGen, gen)
	}

	// --- Retire the strategy (remove from desired) ---
	if _, err := st.SetAssignment("m1", "s", nil); err != nil {
		t.Fatal(err)
	}
	srv.Notify("m1")

	waitFor(t, 15*time.Second, func() bool {
		return !processAlive(int(pid))
	}, "strategy process to be retired")
}

func waitFor(t *testing.T, timeout time.Duration, cond func() bool, what string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// processAlive reports whether pid exists (kill -0 probe). A zombie still
// responds to signal 0, so also reject processes in the zombie state.
func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	if err := syscall.Kill(pid, 0); err != nil {
		return false
	}
	data, err := os.ReadFile(filepath.Join("/proc", itoa(pid), "stat"))
	if err != nil {
		return false
	}
	// state is the field after the ")": "... (comm) STATE ..."
	s := string(data)
	if i := lastByte(s, ')'); i >= 0 && i+2 < len(s) {
		return s[i+2] != 'Z'
	}
	return true
}

func itoa(n int) string { return strconv.Itoa(n) }

func lastByte(s string, b byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] == b {
			return i
		}
	}
	return -1
}
