package driver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestMkdirAllUnderRootfsCreatesNested(t *testing.T) {
	rootfs := t.TempDir()
	got, err := mkdirAllUnderRootfs(rootfs, "/var/lib/mftik")
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(rootfs, "var", "lib", "mftik")
	if got != want {
		t.Fatalf("got %s want %s", got, want)
	}
	st, err := os.Lstat(got)
	if err != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		t.Fatalf("target %#v", st)
	}
}

func TestMkdirAllUnderRootfsRefusesSymlink(t *testing.T) {
	rootfs := t.TempDir()
	if err := os.MkdirAll(filepath.Join(rootfs, "var"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("/run", filepath.Join(rootfs, "var", "run")); err != nil {
		t.Fatal(err)
	}
	_, err := mkdirAllUnderRootfs(rootfs, "/var/run/foo")
	if err == nil || !strings.Contains(err.Error(), "symlink") {
		t.Fatalf("want symlink refusal, got %v", err)
	}
	if _, err := os.Stat(filepath.Join(rootfs, "var", "run", "foo")); !os.IsNotExist(err) {
		t.Fatalf("must not create through the symlink: %v", err)
	}
}

func TestMkdirAllUnderRootfsRejectsRoot(t *testing.T) {
	if _, err := mkdirAllUnderRootfs(t.TempDir(), "/"); err == nil {
		t.Fatal("expected / rejected")
	}
}
