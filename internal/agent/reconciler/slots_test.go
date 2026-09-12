package reconciler

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

func doReap(t *testing.T, r *Reconciler, names ...string) *pb.ReapStrategiesResult {
	t.Helper()
	reply := make(chan *pb.ReapStrategiesResult, 1)
	r.handleReapOp(reapOp{requestID: "t", names: names, reply: reply})
	select {
	case res := <-reply:
		return res
	case done := <-r.reapDoneCh:
		r.applyReapDone(done)
		select {
		case res := <-reply:
			return res
		case <-time.After(2 * time.Second):
			t.Fatal("no reply after reapDone")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("reap timeout")
	}
	return nil
}

func TestBuildSlotStatusListsUndeployedLeftover(t *testing.T) {
	r, _, mgr, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	seedRelease(t, mgr, "probe-fail", "v1")
	if err := mgr.SwitchTo("probe-fail", "v1"); err != nil {
		t.Fatal(err)
	}
	r.walkSlotsNow()
	sr := r.buildStatusReport()
	if sr.GetSlots() == nil || len(sr.GetSlots().GetSlots()) != 1 {
		t.Fatalf("slots = %+v", sr.GetSlots())
	}
	got := sr.GetSlots().GetSlots()[0]
	if got.GetStrategy() != "probe-fail" || got.GetCurrentVersion() != "v1" {
		t.Fatalf("slot = %+v", got)
	}
	if got.GetSizeBytes() <= 0 {
		t.Fatalf("expected walked size, got %d", got.GetSizeBytes())
	}
}

func TestBuildSlotStatusOmitsOnListFailure(t *testing.T) {
	r, _, mgr, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	seedRelease(t, mgr, "probe-fail", "v1")
	notDir := filepath.Join(t.TempDir(), "not-a-dir")
	if err := os.WriteFile(notDir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	mgr.Base = notDir
	sr := r.buildStatusReport()
	if sr.GetSlots() != nil {
		t.Fatalf("list failure must omit slots, got %+v", sr.GetSlots())
	}
}

func TestStatusKeyChangesAfterSlotWalk(t *testing.T) {
	r, _, mgr, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	seedRelease(t, mgr, "s", "v1")
	before := statusKey(r.buildStatusReport())
	r.walkSlotsNow()
	after := statusKey(r.buildStatusReport())
	if before == after {
		t.Fatal("statusKey must change after size walk")
	}
}

func TestReapBlockedWhileDesired(t *testing.T) {
	r, _, mgr, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	seedRelease(t, mgr, "s", "v1")
	r.desired["s"] = assignment("s", "v1", "sha256:aaa", nil)
	res := doReap(t, r, "s")
	if len(res.GetResults()) != 1 || res.GetResults()[0].GetError() != "still assigned" {
		t.Fatalf("results = %+v", res)
	}
	if _, err := os.Stat(mgr.StrategyDir("s")); err != nil {
		t.Fatalf("slot must remain: %v", err)
	}
}

func TestReapBlockedWhileProc(t *testing.T) {
	r, fd, mgr, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	seedRelease(t, mgr, "s", "v1")
	st := newStrategyState("s")
	st.proc = mustStart(t, fd)
	r.actual["s"] = st
	res := doReap(t, r, "s")
	if len(res.GetResults()) != 1 || res.GetResults()[0].GetError() != "process still running" {
		t.Fatalf("results = %+v", res)
	}
}

func TestReapBlockedWhileInflight(t *testing.T) {
	r, _, mgr, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	seedRelease(t, mgr, "s", "v1")
	st := newStrategyState("s")
	st.inflight = &deployOp{target: artRef("v2", "sha256:bbb"), cancel: func() {}}
	r.actual["s"] = st
	res := doReap(t, r, "s")
	if len(res.GetResults()) != 1 || res.GetResults()[0].GetError() != "deploy in progress" {
		t.Fatalf("results = %+v", res)
	}
}

func TestReapAfterRetireDeletesTree(t *testing.T) {
	r, _, mgr, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	seedRelease(t, mgr, "probe-fail", "v1")
	if _, err := mgr.EnsureWorkDir("probe-fail"); err != nil {
		t.Fatal(err)
	}
	st := newStrategyState("probe-fail")
	r.actual["probe-fail"] = st
	r.reconcile() // retire: drop from actual, keep disk
	if _, ok := r.actual["probe-fail"]; ok {
		t.Fatal("retire should drop actual")
	}
	if _, err := os.Stat(mgr.StrategyDir("probe-fail")); err != nil {
		t.Fatalf("undeploy must keep disk: %v", err)
	}
	res := doReap(t, r, "probe-fail")
	if len(res.GetResults()) != 1 || !res.GetResults()[0].GetRemoved() {
		t.Fatalf("reap = %+v", res)
	}
	if _, err := os.Stat(mgr.StrategyDir("probe-fail")); !os.IsNotExist(err) {
		t.Fatalf("slot still present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(mgr.StrategyDir("probe-fail"), "work")); !os.IsNotExist(err) {
		t.Fatal("work dir still present")
	}
}

func TestReconcileWaitsWhileReaping(t *testing.T) {
	r, _, _, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	spec := assignment("s", "v1", "sha256:aaa", &pb.DeployPolicy{Startsecs: 5})
	r.desired["s"] = spec
	st := newStrategyState("s")
	r.actual["s"] = st
	r.reaping["s"] = struct{}{}
	r.reconcile()
	if st.inflight != nil {
		t.Fatal("beginDeploy must wait while reaping")
	}
}

func TestReapRejectsReserved(t *testing.T) {
	r, _, _, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	res := doReap(t, r, "shared")
	if len(res.GetResults()) != 1 || res.GetResults()[0].GetError() != "reserved base name" {
		t.Fatalf("results = %+v", res)
	}
}
