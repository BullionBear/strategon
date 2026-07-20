package filebrowse

import (
	"fmt"
	"os"
	"sort"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"google.golang.org/protobuf/types/known/timestamppb"
)

// List returns a one-level directory listing under root at relPath.
// On failure it returns a DirListing with Error set (never a Go error for
// missing/permission cases) so the control plane can unblock the UI.
func List(root *os.Root, requestID, relPath string) *pb.DirListing {
	out := &pb.DirListing{RequestId: requestID, Path: relPath}
	norm, err := NormalizeRel(relPath)
	if err != nil {
		out.Error = err.Error()
		return out
	}
	out.Path = norm

	dir, err := root.Open(norm)
	if err != nil {
		out.Error = fmt.Sprintf("open: %v", err)
		return out
	}
	entries, err := dir.ReadDir(-1)
	_ = dir.Close()
	if err != nil {
		out.Error = fmt.Sprintf("readdir: %v", err)
		return out
	}
	sort.Slice(entries, func(i, j int) bool {
		return entries[i].Name() < entries[j].Name()
	})
	for _, e := range entries {
		de := &pb.DirEntry{
			Name:  e.Name(),
			IsDir: e.IsDir(),
		}
		info, err := e.Info()
		if err == nil {
			de.Size = info.Size()
			de.ModTime = timestamppb.New(info.ModTime())
			de.IsSymlink = info.Mode()&os.ModeSymlink != 0
			// For symlinks, IsDir reflects the target via FileInfo from ReadDir
			// on most systems; keep Mode-based IsDir from DirEntry.
			if de.IsSymlink {
				de.IsDir = false
			}
		}
		out.Entries = append(out.Entries, de)
	}
	return out
}
