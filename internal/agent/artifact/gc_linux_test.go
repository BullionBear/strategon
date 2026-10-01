//go:build linux

package artifact

import (
	"os"
	"testing"
)

func TestScanProcRootsSeesSelf(t *testing.T) {
	key, ok := statKey("/proc/self/root")
	if !ok {
		t.Fatal("stat /proc/self/root")
	}
	var found bool
	for _, pid := range scanProcRoots()[key] {
		if pid == os.Getpid() {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("pid %d not listed for its own root inode", os.Getpid())
	}
}
