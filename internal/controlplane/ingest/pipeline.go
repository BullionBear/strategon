package ingest

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/artifacturi"
	"github.com/bullionbear/strategon/internal/controlplane/objectstore"
	"github.com/bullionbear/strategon/internal/controlplane/store"
)

// Mode controls which sources are ingested at registration time.
type Mode string

const (
	// ModeCredentialedOnly ingests only when the URI host has a CP credential.
	ModeCredentialedOnly Mode = "credentialed-only"
	// ModeAlways ingests every http(s) source (public too).
	ModeAlways Mode = "always"
)

// ParseMode validates an ingest_mode flag value.
func ParseMode(s string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(s))) {
	case "", ModeCredentialedOnly:
		return ModeCredentialedOnly, nil
	case ModeAlways:
		return ModeAlways, nil
	default:
		return "", fmt.Errorf("unknown ingest mode %q (want credentialed-only|always)", s)
	}
}

// Catalog is the store surface ingest needs.
type Catalog interface {
	FinalizeIngest(name, version, expectedDigest, newURI string) error
	SetArtifactState(name, version, state, reason string) error
	FailPendingArtifacts(reason string) (int, error)
}

// Service runs registration-time ingest: download → sha256 verify → PutObject →
// rewrite catalog uri to s3:// and mark READY (or FAILED).
type Service struct {
	Catalog Catalog
	Objects objectstore.Store
	Creds   *Credentials
	Mode    Mode
	Client  *http.Client
	Logger  *slog.Logger
	TempDir string

	// GitHubAPIBase overrides https://api.github.com (tests).
	GitHubAPIBase string
	ghCache       *assetIDCache
}

// New constructs a Service with sensible defaults.
func New(cat Catalog, objs objectstore.Store, creds *Credentials, mode Mode, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	if creds == nil {
		creds, _ = LoadCredentials("")
	}
	return &Service{
		Catalog: cat,
		Objects: objs,
		Creds:   creds,
		Mode:    mode,
		Client:  newHTTPClient(),
		Logger:  logger,
		ghCache: newAssetIDCache(githubAssetCacheTTL),
	}
}

func newHTTPClient() *http.Client {
	return &http.Client{
		Timeout:       30 * time.Minute,
		CheckRedirect: stripCredentialsOnCrossHostRedirect,
	}
}

// stripCredentialsOnCrossHostRedirect follows redirects like net/http's default
// but also drops custom credential headers (CredHeader / X-API-Key, etc.).
// Go only strips Authorization/Cookie/Proxy-Authorization on host change.
func stripCredentialsOnCrossHostRedirect(req *http.Request, via []*http.Request) error {
	if len(via) >= 10 {
		return errors.New("stopped after 10 redirects")
	}
	if len(via) == 0 {
		return nil
	}
	prev := via[len(via)-1].URL
	if prev == nil || req.URL == nil {
		return nil
	}
	if strings.EqualFold(prev.Hostname(), req.URL.Hostname()) && portOrDefault(prev) == portOrDefault(req.URL) {
		return nil
	}
	// Cross-host: keep only headers safe to forward to an unrelated origin.
	safe := map[string]bool{
		"Accept":          true,
		"Accept-Encoding": true,
		"User-Agent":      true,
		"Range":           true,
	}
	for k := range req.Header {
		if !safe[http.CanonicalHeaderKey(k)] {
			req.Header.Del(k)
		}
	}
	return nil
}

func portOrDefault(u *url.URL) string {
	if p := u.Port(); p != "" {
		return p
	}
	switch u.Scheme {
	case "https":
		return "443"
	case "http":
		return "80"
	default:
		return ""
	}
}

// sourceNeedsIngest is the policy check without requiring an object store.
func (s *Service) sourceNeedsIngest(uri string) bool {
	if s == nil || !artifacturi.IsHTTP(uri) {
		return false
	}
	switch s.Mode {
	case ModeAlways:
		return true
	default: // credentialed-only
		if _, ok := s.Creds.Lookup(uri); ok {
			return true
		}
		// Browser-form GitHub release URLs authenticate via api.github.com.
		if _, ok := ParseGitHubReleaseURL(uri); ok {
			_, ok = s.Creds.LookupHost(githubAPIHost)
			return ok
		}
		return false
	}
}

// NeedsIngest reports whether RegisterArtifact should run the async ingest path.
func (s *Service) NeedsIngest(uri string) bool {
	if s == nil || s.Objects == nil {
		return false
	}
	return s.sourceNeedsIngest(uri)
}

// CheckIngestConfig fails fast when uri would be ingested but S3 is not
// configured — otherwise RegisterArtifact would silently store READY and the
// agent would later GET a credentialed URL with no auth.
func (s *Service) CheckIngestConfig(uri string) error {
	if s == nil || !s.sourceNeedsIngest(uri) {
		return nil
	}
	if s.Objects == nil || s.Objects.Bucket() == "" {
		return fmt.Errorf("ingest requires --s3-endpoint and --s3-bucket (credentialed or always-mode http(s) source)")
	}
	return nil
}

// ValidateSource enforces https-only when a credential will be attached.
func (s *Service) ValidateSource(uri string) error {
	if !artifacturi.IsHTTP(uri) {
		return fmt.Errorf("ingest: only http(s) sources can be ingested, got %q", uri)
	}
	u, err := url.Parse(uri)
	if err != nil {
		return err
	}
	if _, hasCred := s.credentialFor(uri); hasCred && u.Scheme != "https" {
		return fmt.Errorf("ingest: credentialed sources require https:// (got %s)", u.Scheme)
	}
	if s.Objects == nil || s.Objects.Bucket() == "" {
		return fmt.Errorf("ingest: s3 object store with --s3-bucket is required")
	}
	return nil
}

// credentialFor returns the host credential used for uri, including the
// api.github.com bearer used for GitHub release browser URLs.
func (s *Service) credentialFor(uri string) (HostCredential, bool) {
	if cred, ok := s.Creds.Lookup(uri); ok {
		return cred, true
	}
	if _, ok := ParseGitHubReleaseURL(uri); ok {
		return s.Creds.LookupHost(githubAPIHost)
	}
	return HostCredential{}, false
}

// Start runs ingest in a background goroutine. Safe to call after writing PENDING.
//
// Uses context.Background() on purpose: ingest is not tied to the CP request or
// shutdown context. On graceful stop the process may kill in-flight downloads;
// restart then marks leftover PENDING via FailInterrupted, and content-addressed
// PutObject is idempotent for a retry after re-register.
func (s *Service) Start(ref *pb.ArtifactRef) {
	go func() {
		ctx := context.Background()
		if err := s.Run(ctx, ref); err != nil {
			s.Logger.Warn("artifact ingest failed",
				"name", ref.GetName(), "version", ref.GetVersion(), "err", err)
		}
	}()
}

// Run executes the ingest pipeline synchronously (tests / Start).
func (s *Service) Run(ctx context.Context, ref *pb.ArtifactRef) error {
	if ref == nil {
		return fmt.Errorf("ingest: nil artifact")
	}
	name, version, digest := ref.GetName(), ref.GetVersion(), ref.GetDigest()
	uri := ref.GetUri()
	if err := s.ValidateSource(uri); err != nil {
		_ = s.Catalog.SetArtifactState(name, version, store.ArtifactStateFailed, err.Error())
		return err
	}

	tmp, err := os.CreateTemp(s.TempDir, "strategon-ingest-*")
	if err != nil {
		reason := fmt.Sprintf("create temp: %v", err)
		_ = s.Catalog.SetArtifactState(name, version, store.ArtifactStateFailed, reason)
		return fmt.Errorf("ingest: %s", reason)
	}
	tmpPath := tmp.Name()
	defer func() {
		tmp.Close()
		_ = os.Remove(tmpPath)
	}()

	if err := s.download(ctx, uri, tmp); err != nil {
		reason := fmt.Sprintf("download: %v", err)
		_ = s.Catalog.SetArtifactState(name, version, store.ArtifactStateFailed, reason)
		return fmt.Errorf("ingest: %s", reason)
	}
	if err := tmp.Close(); err != nil {
		reason := fmt.Sprintf("close temp: %v", err)
		_ = s.Catalog.SetArtifactState(name, version, store.ArtifactStateFailed, reason)
		return fmt.Errorf("ingest: %s", reason)
	}

	sum, size, err := fileSHA256(tmpPath)
	if err != nil {
		reason := fmt.Sprintf("hash: %v", err)
		_ = s.Catalog.SetArtifactState(name, version, store.ArtifactStateFailed, reason)
		return fmt.Errorf("ingest: %s", reason)
	}
	got := "sha256:" + sum
	if !strings.EqualFold(got, digest) {
		reason := fmt.Sprintf("digest mismatch got %s want %s", got, digest)
		_ = s.Catalog.SetArtifactState(name, version, store.ArtifactStateFailed, reason)
		return fmt.Errorf("ingest: %s", reason)
	}

	f, err := os.Open(tmpPath)
	if err != nil {
		reason := fmt.Sprintf("reopen temp: %v", err)
		_ = s.Catalog.SetArtifactState(name, version, store.ArtifactStateFailed, reason)
		return fmt.Errorf("ingest: %s", reason)
	}
	defer f.Close()

	bucket := s.Objects.Bucket()
	key := objectstore.ObjectKey(name, version, digest)
	if err := s.Objects.PutObject(ctx, bucket, key, f, size); err != nil {
		reason := fmt.Sprintf("upload: %v", err)
		_ = s.Catalog.SetArtifactState(name, version, store.ArtifactStateFailed, reason)
		return fmt.Errorf("ingest: %s", reason)
	}

	newURI := objectstore.ObjectURI(bucket, name, version, digest)
	if err := s.Catalog.FinalizeIngest(name, version, digest, newURI); err != nil {
		// Lost the fence race — another ingest won or digest was replaced.
		// Do not overwrite the winner's READY/PENDING with FAILED.
		if errors.Is(err, store.ErrIngestSuperseded) {
			s.Logger.Info("artifact ingest superseded",
				"name", name, "version", version, "err", err)
			return nil
		}
		reason := fmt.Sprintf("finalize: %v", err)
		_ = s.Catalog.SetArtifactState(name, version, store.ArtifactStateFailed, reason)
		return fmt.Errorf("ingest: %s", reason)
	}
	s.Logger.Info("artifact ingest ready",
		"name", name, "version", version, "uri", newURI)
	return nil
}

// FailInterrupted marks in-flight PENDING rows failed after a CP restart.
func (s *Service) FailInterrupted() {
	if s == nil || s.Catalog == nil {
		return
	}
	n, err := s.Catalog.FailPendingArtifacts("interrupted by restart")
	if err != nil {
		s.Logger.Warn("fail pending artifacts", "err", err)
		return
	}
	if n > 0 {
		s.Logger.Info("marked pending ingest as failed after restart", "count", n)
	}
}

func (s *Service) download(ctx context.Context, rawURL string, dest *os.File) error {
	// Private GitHub release assets need the API path; public browser URLs
	// without api.github.com credentials fall through to a plain GET.
	if ref, ok := ParseGitHubReleaseURL(rawURL); ok {
		if cred, ok := s.Creds.LookupHost(githubAPIHost); ok {
			return s.downloadGitHubRelease(ctx, ref, cred, dest)
		}
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return err
	}
	if cred, ok := s.Creds.Lookup(rawURL); ok {
		applyCredential(req, cred)
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: status %s", rawURL, resp.Status)
	}
	if _, err := io.Copy(dest, resp.Body); err != nil {
		return err
	}
	return nil
}

func applyCredential(req *http.Request, cred HostCredential) {
	switch cred.Type {
	case CredBearer:
		req.Header.Set("Authorization", "Bearer "+cred.Token)
	case CredBasic:
		req.SetBasicAuth(cred.Username, cred.Password)
	case CredHeader:
		req.Header.Set(cred.Header, cred.Value)
	}
}

func fileSHA256(path string) (sum string, size int64, err error) {
	f, err := os.Open(path)
	if err != nil {
		return "", 0, err
	}
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(h, f)
	if err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
