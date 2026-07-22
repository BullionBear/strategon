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

func TestReconcileSharedBeforeAssignments(t *testing.T) {
	r, _, mgr, _, _ := newTestReconciler(t, time.Unix(1000, 0))
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
	// Pre-seed release so deploy can proceed after shared; we only assert shared ordering
	// by checking shared is present after one reconcile pass that also sees assignments.
	seedRelease(t, mgr, "s", "v1")
	_ = mgr.SwitchTo("s", "v1")

	r.reconcile()

	if got := mgr.RunningSharedDigest("instruments.json"); got != ref.Digest {
		t.Fatalf("shared not converged: got %q want %q", got, ref.Digest)
	}
	st := r.buildSharedStatus()
	if st == nil || st.GetObservedGeneration() != 1 || len(st.GetFiles()) != 1 {
		t.Fatalf("shared status = %+v", st)
	}
	if st.GetFiles()[0].GetRunningDigest() != ref.Digest || st.GetFiles()[0].GetLastError() != "" {
		t.Fatalf("file status = %+v", st.GetFiles()[0])
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
	if mgr.RunningSharedDigest("bad.json") != "" {
		t.Fatal("must not switch on digest mismatch")
	}
	st := r.sharedActual["bad.json"]
	if st == nil || st.lastError == "" {
		t.Fatalf("expected last_error, got %+v", st)
	}
}
