package artifact

import (
	"os"
	"path/filepath"
	"testing"
)

func TestListStrategyDirsSkipsReservedAndFiles(t *testing.T) {
	base := t.TempDir()
	mgr := NewManager(base, nil)
	for _, name := range []string{"shared", "volumes", "agent", "ok-strat", "nats-m1"} {
		if err := os.MkdirAll(filepath.Join(base, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(base, "not-a-dir"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	got, err := mgr.ListStrategyDirs()
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0] != "nats-m1" || got[1] != "ok-strat" {
		t.Fatalf("ListStrategyDirs = %v", got)
	}
}

func TestDirSizeIgnoresSymlinks(t *testing.T) {
	base := t.TempDir()
	slot := filepath.Join(base, "s")
	rel := filepath.Join(slot, "releases", "v1")
	if err := os.MkdirAll(rel, 0o755); err != nil {
		t.Fatal(err)
	}
	payload := make([]byte, 1024)
	if err := os.WriteFile(filepath.Join(rel, "bin"), payload, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("releases/v1", filepath.Join(slot, "current")); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "shared-payload")
	if err := os.WriteFile(outside, make([]byte, 50_000), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(slot, "shared")); err != nil {
		t.Fatal(err)
	}

	n, err := DirSize(slot)
	if err != nil {
		t.Fatal(err)
	}
	if n >= 50_000 {
		t.Fatalf("DirSize followed symlink: %d", n)
	}
	if n < 1024 {
		t.Fatalf("DirSize = %d, want at least the 1024-byte bin", n)
	}
}

func TestRemoveStrategyDirRejectsReserved(t *testing.T) {
	base := t.TempDir()
	mgr := NewManager(base, nil)
	shared := filepath.Join(base, "shared")
	if err := os.MkdirAll(shared, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := mgr.RemoveStrategyDir("shared"); err == nil {
		t.Fatal("expected reserved name error")
	}
	if _, err := os.Stat(shared); err != nil {
		t.Fatalf("reserved dir must remain: %v", err)
	}
	if err := mgr.RemoveStrategyDir("../escape"); err == nil {
		t.Fatal("expected invalid name error")
	}
}

func TestRemoveStrategyDirDeletesSlot(t *testing.T) {
	base := t.TempDir()
	mgr := NewManager(base, nil)
	slot := mgr.StrategyDir("probe-fail")
	if err := os.MkdirAll(filepath.Join(slot, "work"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := mgr.RemoveStrategyDir("probe-fail"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(slot); !os.IsNotExist(err) {
		t.Fatalf("slot still present: %v", err)
	}
	if err := mgr.RemoveStrategyDir("already-gone"); err != nil {
		t.Fatalf("missing dir should succeed: %v", err)
	}
}
