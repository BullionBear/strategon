package ingest

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	githubAPIHost     = "api.github.com"
	defaultGitHubAPI  = "https://api.github.com"
	githubAssetCacheTTL = 5 * time.Minute
)

// GitHubReleaseRef is a browser-form GitHub Releases download URL, parsed into
// the pieces needed for the releases API.
//
//	https://github.com/{Owner}/{Repo}/releases/download/{Tag}/{Asset}
type GitHubReleaseRef struct {
	Owner string
	Repo  string
	Tag   string
	Asset string // asset filename
}

// ParseGitHubReleaseURL recognizes the browser download form of a GitHub
// release asset. Other github.com URLs (and non-GitHub hosts) return ok=false.
func ParseGitHubReleaseURL(raw string) (GitHubReleaseRef, bool) {
	u, err := url.Parse(raw)
	if err != nil || (u.Scheme != "https" && u.Scheme != "http") {
		return GitHubReleaseRef{}, false
	}
	host := strings.ToLower(u.Hostname())
	if host != "github.com" && host != "www.github.com" {
		return GitHubReleaseRef{}, false
	}
	// /{owner}/{repo}/releases/download/{tag}/{asset...}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) < 6 {
		return GitHubReleaseRef{}, false
	}
	if parts[2] != "releases" || parts[3] != "download" {
		return GitHubReleaseRef{}, false
	}
	owner, repo, tag := parts[0], parts[1], parts[4]
	asset := strings.Join(parts[5:], "/")
	if owner == "" || repo == "" || tag == "" || asset == "" {
		return GitHubReleaseRef{}, false
	}
	// Percent-decode tag/asset (tags rarely need it; assets may).
	if dec, err := url.PathUnescape(tag); err == nil {
		tag = dec
	}
	if dec, err := url.PathUnescape(asset); err == nil {
		asset = dec
	}
	return GitHubReleaseRef{Owner: owner, Repo: repo, Tag: tag, Asset: asset}, true
}

type assetCacheEntry struct {
	id      int64
	expires time.Time
}

// assetIDCache is a short-lived tag→asset-id map (authenticated GitHub rate
// limit is 5000/hr; ingest is low-frequency, but caching is cheap).
type assetIDCache struct {
	mu    sync.Mutex
	items map[string]assetCacheEntry
	ttl   time.Duration
	now   func() time.Time
}

func newAssetIDCache(ttl time.Duration) *assetIDCache {
	return &assetIDCache{
		items: map[string]assetCacheEntry{},
		ttl:   ttl,
		now:   time.Now,
	}
}

func (c *assetIDCache) key(ref GitHubReleaseRef) string {
	return ref.Owner + "/" + ref.Repo + "@" + ref.Tag + "/" + ref.Asset
}

func (c *assetIDCache) get(ref GitHubReleaseRef) (int64, bool) {
	if c == nil {
		return 0, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e, ok := c.items[c.key(ref)]
	if !ok || !c.now().Before(e.expires) {
		return 0, false
	}
	return e.id, true
}

func (c *assetIDCache) put(ref GitHubReleaseRef, id int64) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.items[c.key(ref)] = assetCacheEntry{id: id, expires: c.now().Add(c.ttl)}
}

// LookupHost returns credentials for an exact host key (e.g. api.github.com).
func (c *Credentials) LookupHost(host string) (HostCredential, bool) {
	if c == nil {
		return HostCredential{}, false
	}
	cred, ok := c.byHost[strings.ToLower(strings.TrimSpace(host))]
	return cred, ok
}

// githubAPIBase is overridable in tests (httptest). Empty → defaultGitHubAPI.
func (s *Service) githubAPIBase() string {
	if s != nil && s.GitHubAPIBase != "" {
		return strings.TrimRight(s.GitHubAPIBase, "/")
	}
	return defaultGitHubAPI
}

func (s *Service) githubAssetCache() *assetIDCache {
	if s.ghCache == nil {
		s.ghCache = newAssetIDCache(githubAssetCacheTTL)
	}
	return s.ghCache
}

// downloadGitHubRelease resolves a browser release URL through the GitHub API
// (required for private repos) and streams the asset bytes. Callers must only
// invoke this when a bearer credential for api.github.com is present.
//
// Redirects to objects.githubusercontent.com are followed by net/http, which
// strips Authorization on cross-host redirects — do not intercept 302s.
func (s *Service) downloadGitHubRelease(ctx context.Context, ref GitHubReleaseRef, cred HostCredential, dest *os.File) error {
	if cred.Type != CredBearer || cred.Token == "" {
		return fmt.Errorf("github ingest: api.github.com credential must be type bearer")
	}
	assetID, err := s.resolveGitHubAssetID(ctx, ref, cred.Token)
	if err != nil {
		return err
	}
	assetURL := fmt.Sprintf("%s/repos/%s/%s/releases/assets/%d",
		s.githubAPIBase(), url.PathEscape(ref.Owner), url.PathEscape(ref.Repo), assetID)

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, assetURL, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("Authorization", "Bearer "+cred.Token)
	// Default CheckRedirect: follow 302 and strip Authorization cross-host.

	resp, err := s.Client.Do(req)
	if err != nil {
		return fmt.Errorf("github asset download: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("github asset download: status %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	if _, err := io.Copy(dest, resp.Body); err != nil {
		return fmt.Errorf("github asset download: copy: %w", err)
	}
	return nil
}

type ghRelease struct {
	Assets []ghAsset `json:"assets"`
}

type ghAsset struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

func (s *Service) resolveGitHubAssetID(ctx context.Context, ref GitHubReleaseRef, token string) (int64, error) {
	if id, ok := s.githubAssetCache().get(ref); ok {
		return id, nil
	}
	apiURL := fmt.Sprintf("%s/repos/%s/%s/releases/tags/%s",
		s.githubAPIBase(),
		url.PathEscape(ref.Owner),
		url.PathEscape(ref.Repo),
		url.PathEscape(ref.Tag),
	)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, apiURL, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := s.Client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("github releases/tags: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return 0, fmt.Errorf("github releases/tags: status %s: %s", resp.Status, strings.TrimSpace(string(body)))
	}
	var rel ghRelease
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return 0, fmt.Errorf("github releases/tags: decode: %w", err)
	}
	for _, a := range rel.Assets {
		if a.Name == ref.Asset {
			s.githubAssetCache().put(ref, a.ID)
			return a.ID, nil
		}
	}
	return 0, fmt.Errorf("github releases/tags: asset %q not found in %s/%s@%s",
		ref.Asset, ref.Owner, ref.Repo, ref.Tag)
}
