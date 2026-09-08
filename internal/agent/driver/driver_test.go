package driver

import (
	"path/filepath"
	"testing"
)

func TestOCIInitLogPathSitsBesideWorkDir(t *testing.T) {
	work := filepath.Join(string(filepath.Separator), "opt", "strategies", "s", "work")
	got := OCIInitLogPath(work)
	want := filepath.Join(string(filepath.Separator), "opt", "strategies", "s", OCIInitLogName)
	if got != want {
		t.Fatalf("OCIInitLogPath(%q) = %q, want %q", work, got, want)
	}
}
