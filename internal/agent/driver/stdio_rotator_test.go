package driver

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestSizeRotatorRotatesAndKeepsArchives(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenSizeRotator(dir, PayloadLogName, 100, 2)
	if err != nil {
		t.Fatal(err)
	}
	r.SetMarker(StdioStartMarker(1, time.Unix(0, 0).UTC(), "v1"))
	defer r.Close()

	if _, err := r.Write([]byte(strings.Repeat("a", 40))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte(strings.Repeat("b", 40))); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte(strings.Repeat("c", 40))); err != nil {
		t.Fatal(err)
	}

	cur, err := os.ReadFile(filepath.Join(dir, PayloadLogName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(cur), "c") {
		t.Fatalf("current = %q", cur)
	}
	if !strings.Contains(string(cur), "=== start pid=1") {
		t.Fatalf("rotated current missing marker: %q", cur)
	}
	if _, err := os.Stat(filepath.Join(dir, PayloadLogName+".1")); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte(strings.Repeat("d", 40))); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, PayloadLogName+".2")); err != nil {
		t.Fatal(err)
	}
}

func TestSizeRotatorAppendDoesNotTruncate(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenSizeRotator(dir, PayloadLogName, 1<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := r.Write([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	r.Close()

	r2, err := OpenSizeRotator(dir, PayloadLogName, 1<<20, 3)
	if err != nil {
		t.Fatal(err)
	}
	defer r2.Close()
	if _, err := r2.Write([]byte("second\n")); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(filepath.Join(dir, PayloadLogName))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "first\n") || !strings.Contains(string(body), "second\n") {
		t.Fatalf("got %q", body)
	}
}

func TestSizeRotatorWriteErrorDiscards(t *testing.T) {
	dir := t.TempDir()
	r, err := OpenSizeRotator(dir, PayloadLogName, 1<<20, 1)
	if err != nil {
		t.Fatal(err)
	}
	defer r.Close()
	_ = r.f.Close()
	r.f = nil
	// Closed handle: force a write error path via a closed pipe.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	pr.Close()
	r.f = pw
	n, err := r.Write([]byte("nope"))
	if err != nil || n != 4 {
		t.Fatalf("discard write: n=%d err=%v", n, err)
	}
	if !r.discard {
		t.Fatal("expected discard after write error")
	}
	n, err = r.Write([]byte("still-ok"))
	if err != nil || n != 8 {
		t.Fatalf("discarded write: n=%d err=%v", n, err)
	}
}

func TestPayloadLogDirBesideStrategyDir(t *testing.T) {
	got := PayloadLogDir("/opt/strategies/s")
	if got != "/opt/strategies/s/"+StdioDirName {
		t.Fatalf("%q", got)
	}
}
