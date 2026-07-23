package reconciler

import (
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
)

func sharedRef(t *testing.T, srcDir, name, body string) *pb.ArtifactRef {
	t.Helper()
	path := filepath.Join(srcDir, name)
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(body))
	return &pb.ArtifactRef{
		Name:    name,
		Version: "v1",
		Digest:  "sha256:" + hex.EncodeToString(sum[:]),
		Uri:     "file://" + path,
	}
}

// waitSharedWorker drains one shared fetch event and applies it on the main
// path (unit tests call reconcile() without Run).
func waitSharedWorker(t *testing.T, r *Reconciler) {
	t.Helper()
	select {
	case ev := <-r.sharedCh:
		r.applySharedWorkerEvent(ev)
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for shared worker event")
	}
}

func TestReconcileSharedBeforeAssignments(t *testing.T) {
	r, fd, mgr, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	src := t.TempDir()
	ref := sharedRef(t, src, "instruments.json", `{"ok":true}`)

	// Desired shared + an assignment that would start after shared converges.
	r.applyDesired(&pb.DesiredState{
		Generation: 1,
		Shared: &pb.MachineSharedSpec{
			Generation: 1,
			Files:      []*pb.SharedFileSpec{{Name: "instruments.json", Artifact: ref}},
		},
		Assignments: []*pb.StrategyAssignmentSpec{
			assignment("s", "v1", "sha256:aaa", &pb.DeployPolicy{Startsecs: 5}),
		},
	})
	// Pre-seed release + pretend current version already matches so reconcile
	// takes the startProcess path (not a full deploy) once shared is ready.
	seedRelease(t, mgr, "s", "v1")
	_ = mgr.SwitchTo("s", "v1")
	ast := newStrategyState("s")
	ast.runningArtifact = artRef("v1", "sha256:aaa")
	r.actual["s"] = ast

	// First pass: fetch is initiated but not yet applied — assignment must not start.
	r.reconcile()
	if r.sharedPresent() {
		t.Fatal("shared should be absent before worker completes")
	}
	if fd.starts() != 0 {
		t.Fatalf("assignment started before shared present: %d starts", fd.starts())
	}

	waitSharedWorker(t, r)
	r.reconcile()

	if got := mgr.RunningSharedDigest("instruments.json"); got != ref.Digest {
		t.Fatalf("shared not present: got %q want %q", got, ref.Digest)
	}
	if fd.starts() == 0 {
		t.Fatal("expected assignment start after shared present")
	}
	st := r.buildSharedStatus()
	if st == nil || st.GetObservedGeneration() != 1 || len(st.GetFiles()) != 1 {
		t.Fatalf("shared status = %+v", st)
	}
	if st.GetFiles()[0].GetRunningDigest() != ref.Digest || st.GetFiles()[0].GetLastError() != "" {
		t.Fatalf("file status = %+v", st.GetFiles()[0])
	}
}

func TestSharedGateAllowsStaleDigest(t *testing.T) {
	r, fd, mgr, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	src := t.TempDir()
	ref1 := sharedRef(t, src, "instruments.json", `v1`)
	path2 := filepath.Join(src, "instruments-v2.json")
	if err := os.WriteFile(path2, []byte(`v2`), 0o644); err != nil {
		t.Fatal(err)
	}
	// Bad digest so the v2 fetch fails and stays stale forever.
	ref2 := &pb.ArtifactRef{
		Name: "instruments.json", Version: "v2",
		Digest: "sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		Uri:    "file://" + path2,
	}

	r.applyDesired(&pb.DesiredState{
		Generation: 1,
		Shared: &pb.MachineSharedSpec{
			Generation: 1,
			Files:      []*pb.SharedFileSpec{{Name: "instruments.json", Artifact: ref1}},
		},
		Assignments: []*pb.StrategyAssignmentSpec{
			assignment("s", "v1", "sha256:aaa", &pb.DeployPolicy{Startsecs: 5}),
		},
	})
	seedRelease(t, mgr, "s", "v1")
	_ = mgr.SwitchTo("s", "v1")
	ast := newStrategyState("s")
	ast.runningArtifact = artRef("v1", "sha256:aaa")
	r.actual["s"] = ast

	r.reconcile()
	waitSharedWorker(t, r)
	r.reconcile()
	startsAfterV1 := fd.starts()
	if startsAfterV1 == 0 {
		t.Fatal("expected start once v1 is present")
	}

	// Push a broken v2: running stays v1 (stale). Starts must still be allowed.
	r.applyDesired(&pb.DesiredState{
		Generation: 2,
		Shared: &pb.MachineSharedSpec{
			Generation: 2,
			Files:      []*pb.SharedFileSpec{{Name: "instruments.json", Artifact: ref2}},
		},
		Assignments: []*pb.StrategyAssignmentSpec{
			assignment("s", "v1", "sha256:aaa", &pb.DeployPolicy{Startsecs: 5}),
		},
	})
	// Simulate crash: process gone, same desired version → startProcess path.
	ast.proc = nil
	ast.phase = pb.DeployPhase_DEPLOY_PHASE_HEALTHY
	r.reconcile()
	waitSharedWorker(t, r) // digest mismatch failure
	if !r.sharedPresent() {
		t.Fatal("stale v1 must count as present")
	}
	r.reconcile()
	if fd.starts() <= startsAfterV1 {
		t.Fatalf("stale shared must not block crash restart; starts before=%d after=%d",
			startsAfterV1, fd.starts())
	}
}

func TestReconcileSharedStaleDigestAndRemoval(t *testing.T) {
	r, _, mgr, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	src := t.TempDir()
	ref1 := sharedRef(t, src, "instruments.json", `v1`)
	path2 := filepath.Join(src, "instruments-v2.json")
	if err := os.WriteFile(path2, []byte(`v2body`), 0o644); err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256([]byte(`v2body`))
	ref2 := &pb.ArtifactRef{
		Name: "instruments.json", Version: "v2",
		Digest: "sha256:" + hex.EncodeToString(sum[:]), Uri: "file://" + path2,
	}

	r.applyDesired(&pb.DesiredState{
		Generation: 1,
		Shared: &pb.MachineSharedSpec{
			Generation: 1,
			Files:      []*pb.SharedFileSpec{{Name: "instruments.json", Artifact: ref1}},
		},
	})
	r.reconcile()
	waitSharedWorker(t, r)
	if mgr.RunningSharedDigest("instruments.json") != ref1.Digest {
		t.Fatal("expected v1")
	}

	r.applyDesired(&pb.DesiredState{
		Generation: 2,
		Shared: &pb.MachineSharedSpec{
			Generation: 2,
			Files:      []*pb.SharedFileSpec{{Name: "instruments.json", Artifact: ref2}},
		},
	})
	r.reconcile()
	waitSharedWorker(t, r)
	if got := mgr.RunningSharedDigest("instruments.json"); got != ref2.Digest {
		t.Fatalf("stale re-converge: got %q want %q", got, ref2.Digest)
	}

	// Remove file from desired → unlink.
	r.applyDesired(&pb.DesiredState{
		Generation: 3,
		Shared:     &pb.MachineSharedSpec{Generation: 3, Files: nil},
	})
	r.reconcile()
	if _, err := os.Lstat(mgr.SharedLink("instruments.json")); !os.IsNotExist(err) {
		t.Fatalf("symlink should be removed, err=%v", err)
	}
}

func TestReconcileSharedDigestMismatch(t *testing.T) {
	r, _, mgr, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	src := t.TempDir()
	path := filepath.Join(src, "bad.json")
	if err := os.WriteFile(path, []byte("real"), 0o644); err != nil {
		t.Fatal(err)
	}
	ref := &pb.ArtifactRef{
		Name: "bad.json", Version: "v1",
		Digest: "sha256:deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef",
		Uri:    "file://" + path,
	}
	r.applyDesired(&pb.DesiredState{
		Generation: 1,
		Shared: &pb.MachineSharedSpec{
			Generation: 1,
			Files:      []*pb.SharedFileSpec{{Name: "bad.json", Artifact: ref}},
		},
	})
	r.reconcile()
	waitSharedWorker(t, r)
	if mgr.RunningSharedDigest("bad.json") != "" {
		t.Fatal("must not switch on digest mismatch")
	}
	st := r.sharedActual["bad.json"]
	if st == nil || st.lastError == "" {
		t.Fatalf("expected last_error, got %+v", st)
	}
}

func TestBuildSharedStatusReportsRemovalFailure(t *testing.T) {
	r, _, mgr, _, _ := newTestReconciler(t, time.Unix(1000, 0))
	src := t.TempDir()
	ref := sharedRef(t, src, "instruments.json", `x`)
	r.applyDesired(&pb.DesiredState{
		Generation: 1,
		Shared: &pb.MachineSharedSpec{
			Generation: 1,
			Files:      []*pb.SharedFileSpec{{Name: "instruments.json", Artifact: ref}},
		},
	})
	r.reconcile()
	waitSharedWorker(t, r)

	// Simulate a stuck removal: leave actual state with an error and no desired.
	r.applyDesired(&pb.DesiredState{
		Generation: 2,
		Shared:     &pb.MachineSharedSpec{Generation: 2, Files: nil},
	})
	st := r.sharedActual["instruments.json"]
	if st == nil {
		t.Fatal("expected actual state")
	}
	// Make RemoveSharedLink fail by replacing the symlink with a non-empty dir
	// of the same name (Remove returns EISDIR / not empty on some systems).
	link := mgr.SharedLink("instruments.json")
	_ = os.Remove(link)
	if err := os.Mkdir(link, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(link, "blocker"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	r.reconcile()
	status := r.buildSharedStatus()
	if status == nil {
		t.Fatal("expected status")
	}
	found := false
	for _, f := range status.GetFiles() {
		if f.GetName() == "instruments.json" && f.GetLastError() != "" {
			found = true
		}
	}
	if !found {
		t.Fatalf("removal failure must appear in status: %+v", status.GetFiles())
	}
}
