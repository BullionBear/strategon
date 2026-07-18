// Package artifacturi resolves local artifact URIs used by RegisterArtifact and
// the agent's LocalFetcher. Foundation deploys use file:///abs/path (or a bare
// absolute path); S3/MinIO URIs are a deferred follow-up.
package artifacturi

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// ResolveLocal turns a local artifact URI into an absolute filesystem path.
// Accepted forms: absolute path ("/tmp/x") or file URL with an absolute path
// ("file:///tmp/x", "file://localhost/tmp/x"). The common mistake
// "file://tmp/x" (two slashes → host "tmp", relative) is rejected with a
// pointed error instead of becoming a cwd-relative open.
func ResolveLocal(uri string) (string, error) {
	if uri == "" {
		return "", fmt.Errorf("empty uri")
	}
	if strings.HasPrefix(uri, "/") {
		return filepath.Clean(uri), nil
	}
	if !strings.HasPrefix(uri, "file:") {
		return "", fmt.Errorf("unsupported uri %q (want file:///abs/path or absolute path)", uri)
	}
	u, err := url.Parse(uri)
	if err != nil {
		return "", fmt.Errorf("parse uri %q: %w", uri, err)
	}
	host := u.Host
	path := u.Path
	if host != "" && host != "localhost" {
		// file://tmp/foo → host=tmp, path=/foo. Almost always a missing slash.
		return "", fmt.Errorf("uri %q is not a local absolute path (host %q); use file:///%s%s (three slashes after file:)",
			uri, host, host, path)
	}
	if path == "" || !strings.HasPrefix(path, "/") {
		return "", fmt.Errorf("uri %q has no absolute path; use file:///abs/path", uri)
	}
	return filepath.Clean(path), nil
}
