// Package artifacturi resolves and validates artifact URIs used by
// RegisterArtifact and the agent fetchers. Supported sources: http(s) URLs
// (e.g. a GitHub Releases asset), s3://bucket/key (resolved by the control
// plane to a short-lived presigned HTTPS URL), file:///abs/path, or a bare
// absolute path. Integrity is enforced separately by the agent, which
// re-hashes the fetched bytes against ref.digest before switching, so the
// transport is untrusted.
package artifacturi

import (
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
)

// IsHTTP reports whether uri is an http(s) URL (fetched by the HTTP fetcher).
func IsHTTP(uri string) bool {
	return strings.HasPrefix(uri, "http://") || strings.HasPrefix(uri, "https://")
}

// IsS3 reports whether uri is an s3://bucket/key object reference.
func IsS3(uri string) bool {
	return strings.HasPrefix(uri, "s3://")
}

// S3Object is the bucket + key parsed from an s3:// URI.
type S3Object struct {
	Bucket string
	Key    string
}

// ParseS3 splits an s3://bucket/key URI into bucket and object key.
// The key may contain slashes; a trailing slash-only key is rejected.
func ParseS3(uri string) (S3Object, error) {
	if !IsS3(uri) {
		return S3Object{}, fmt.Errorf("not an s3 uri: %q", uri)
	}
	u, err := url.Parse(uri)
	if err != nil {
		return S3Object{}, fmt.Errorf("parse uri %q: %w", uri, err)
	}
	bucket := u.Host
	if bucket == "" {
		return S3Object{}, fmt.Errorf("uri %q has no bucket", uri)
	}
	key := strings.TrimPrefix(u.Path, "/")
	if key == "" {
		return S3Object{}, fmt.Errorf("uri %q has no object key", uri)
	}
	if strings.Contains(key, "//") || strings.HasPrefix(key, "/") {
		return S3Object{}, fmt.Errorf("uri %q has invalid object key", uri)
	}
	return S3Object{Bucket: bucket, Key: key}, nil
}

// Validate checks that uri is a supported artifact source without fetching it:
// an http(s) URL with a host, an s3://bucket/key, a file URL with an absolute
// path, or a bare absolute path. Used by RegisterArtifact to reject bad URIs
// at catalog time.
func Validate(uri string) error {
	if uri == "" {
		return fmt.Errorf("empty uri")
	}
	if IsHTTP(uri) {
		u, err := url.Parse(uri)
		if err != nil {
			return fmt.Errorf("parse uri %q: %w", uri, err)
		}
		if u.Host == "" {
			return fmt.Errorf("uri %q has no host", uri)
		}
		return nil
	}
	if IsS3(uri) {
		_, err := ParseS3(uri)
		return err
	}
	_, err := ResolveLocal(uri)
	return err
}

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
