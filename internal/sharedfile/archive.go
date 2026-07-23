package sharedfile

import (
	"fmt"
	"net/url"
	"path"
	"strings"
)

// archiveSuffixes are extensions that imply a packed blob. Shared files are
// single-file-only today; pointing SetSharedFiles at one of these would symlink
// opaque archive bytes and the consumer would read garbage with no agent error.
// Directory/tarball extraction is deferred (§5); reject at ingress until then.
var archiveSuffixes = []string{
	".tar.gz", ".tgz",
	".tar.bz2", ".tbz2", ".tbz",
	".tar.xz", ".txz",
	".tar.zst", ".tzst",
	".tar",
	".zip",
	".7z",
	".rar",
}

// LooksLikeArchive reports whether s (a basename or URI/path) ends with a
// known archive suffix. Query/fragment are stripped for URIs.
func LooksLikeArchive(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	base := archiveBasename(s)
	lower := strings.ToLower(base)
	for _, suf := range archiveSuffixes {
		if strings.HasSuffix(lower, suf) {
			return true
		}
	}
	return false
}

func archiveBasename(s string) string {
	// Prefer URL path when the string parses as a URI with a scheme.
	if u, err := url.Parse(s); err == nil && u.Scheme != "" && u.Path != "" {
		return path.Base(u.Path)
	}
	// file:///abs or bare path / abs with query.
	if i := strings.IndexAny(s, "?#"); i >= 0 {
		s = s[:i]
	}
	s = strings.TrimPrefix(s, "file://")
	return path.Base(strings.ReplaceAll(s, "\\", "/"))
}

// RejectArchiveRef returns an error when the on-disk shared name or the
// artifact catalog name/URI looks like an archive. Call from SetSharedFiles.
func RejectArchiveRef(sharedName string, artifactName, artifactURI string) error {
	for _, candidate := range []struct {
		label string
		value string
	}{
		{"shared file name", sharedName},
		{"artifact name", artifactName},
		{"artifact uri", artifactURI},
	} {
		if LooksLikeArchive(candidate.value) {
			return fmt.Errorf("%s %q looks like an archive; shared files are single-file only (directory/tarball support is not enabled)",
				candidate.label, candidate.value)
		}
	}
	return nil
}
