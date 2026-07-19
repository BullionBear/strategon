package reconciler

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

// TestCronRestartRerendersConfigPlaceholder proves the config/binary
// separation feature and the cron scheduler compose correctly: a
// CRON_ACTION_RESTART drain+respawn must re-render ${CONFIG} against the
// *current* release, exactly like the initial deploy does (RECONCILER §6.1,
// ARCHITECTURE §8.4, §10).
func TestCronRestartRerendersConfigPlaceholder(t *testing.T) {
	t0 := time.Date(2024, 6, 1, 12, 0, 0, 0, time.UTC)
	r, fd, mgr, _, _ := newTestReconciler(t, t0)
	r.deps.CronRand = func(n int32) int32 { return 0 }

	src := t.TempDir()
	binPath := filepath.Join(src, "bin")
	cfgPath := filepath.Join(src, "app.yml")
	if err := os.WriteFile(binPath, []byte("#!/bin/true\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("k: v\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	art := &pb.ArtifactRef{Version: "v1", Digest: "sha256:x", Uri: "file://" + binPath}
	cfg := &pb.ArtifactRef{Version: "c1", Digest: "sha256:y", Uri: "file://" + cfgPath}
	if err := mgr.Download(context.Background(), "s", art, cfg); err != nil {
		t.Fatal(err)
	}
	if err := mgr.SwitchTo("s", "v1"); err != nil {
		t.Fatal(err)
	}

	spec := &pb.StrategyAssignmentSpec{
		Strategy:     "s",
		Artifact:     art,
		Config:       cfg,
		Args:         []string{"-c", "${CONFIG}"},
		Env:          map[string]string{"FOO": "bar"},
		DeployPolicy: &pb.DeployPolicy{Startsecs: 5, StopGraceSeconds: 1},
		Schedules: []*pb.CronSchedule{{
			Name:     "restart",
			CronExpr: "0 0 * * *",
			Timezone: "UTC",
			Action:   pb.CronAction_CRON_ACTION_RESTART,
		}},
	}
	r.desired = map[string]*pb.StrategyAssignmentSpec{"s": spec}
	st := newStrategyState("s")
	st.phase = pb.DeployPhase_DEPLOY_PHASE_HEALTHY
	st.runningArtifact = art
	st.runningConfig = cfg
	proc := mustStart(t, fd)
	st.proc = proc
	r.actual["s"] = st

	wantCfg, err := filepath.Abs(mgr.CurrentConfigPath("s", cfg))
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(wantCfg) != "config.yml" {
		t.Fatalf("config basename = %q, want config.yml", filepath.Base(wantCfg))
	}

	// Prime the schedule, then fire the RESTART cron action.
	r.tickCron(t0)
	due := st.cron["restart"].nextFire
	r.tickCron(due)
	if st.phase != pb.DeployPhase_DEPLOY_PHASE_DRAINING || !st.stopping {
		t.Fatalf("phase=%v stopping=%v, want DRAINING/stopping", st.phase, st.stopping)
	}

	// Unblock gracefulStop + exit watcher (mirrors TestCronRestartDrains).
	fd.kill(proc.PID)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && st.proc != nil {
		select {
		case ex := <-r.exitCh:
			r.handleExit(ex)
		default:
			time.Sleep(10 * time.Millisecond)
		}
	}
	if st.proc != nil {
		r.handleExit(processExit{strategy: "s", info: exitAt(proc, t0.Add(time.Second))})
	}
	r.reconcile()

	if fd.starts() < 2 {
		t.Fatalf("expected respawn after cron RESTART, starts=%d", fd.starts())
	}
	last := fd.started[len(fd.started)-1]
	if len(last.Args) != 2 || last.Args[0] != "-c" || last.Args[1] != wantCfg {
		t.Fatalf("respawn args = %#v, want [-c %s]", last.Args, wantCfg)
	}
	foundEnv := false
	for _, kv := range last.Env {
		if kv == "FOO=bar" {
			foundEnv = true
		}
	}
	if !foundEnv {
		t.Fatalf("respawn env = %#v, want FOO=bar preserved", last.Env)
	}
}
