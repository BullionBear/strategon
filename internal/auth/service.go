package auth

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/bullionbear/strategon/internal/controlplane/store"
)

// Config configures the human-API auth service.
type Config struct {
	Mode Mode

	// SessionSecret HMAC-signs cookies. If empty, a random secret is generated
	// (sessions do not survive process restart).
	SessionSecret string

	// MockUser is the injected/mock principal username (ModeNone / ModeMock).
	MockUser string
	MockID   string

	// Discord OAuth (required when Mode == ModeDiscord).
	DiscordClientID     string
	DiscordClientSecret string
	DiscordRedirectURL  string // e.g. http://127.0.0.1:8081/auth/callback

	// DiscordGuildID restricts login to members of one Discord guild.
	// Authorization is flat — any principal who logs in is a full operator —
	// so on a public deployment this is the only thing standing between the
	// internet and SetDeployment. Empty means any Discord account may log in.
	DiscordGuildID string

	// FrontendURL is where browsers return after login/logout.
	FrontendURL string

	// Store persists API tokens (create/revoke write-through; LastUsed batched).
	// Nil keeps tokens in-process only (unit tests). Production should pass the
	// control-plane Store (Postgres for durability; Memory for --db="").
	Store store.Store

	Logger *slog.Logger
}

// Service owns session signing, Discord OAuth, and API tokens.
type Service struct {
	mode                Mode
	sessionSecret       []byte
	mockUser            *User
	discordClientID     string
	discordClientSecret string
	discordRedirectURL  string
	discordGuildID      string
	frontendURL         string
	tokens              *tokenStore
	exchanges           *exchangeStore
	logger              *slog.Logger
}

// New validates cfg and builds a Service.
func New(cfg Config) (*Service, error) {
	mode := cfg.Mode
	if mode == "" {
		mode = ModeNone
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	secret := []byte(strings.TrimSpace(cfg.SessionSecret))
	if len(secret) == 0 {
		var err error
		secret, err = randomSecret()
		if err != nil {
			return nil, err
		}
		if mode != ModeNone {
			logger.Warn("auth session secret not set; generated ephemeral secret (sessions reset on restart)")
		}
	}

	mockName := strings.TrimSpace(cfg.MockUser)
	if mockName == "" {
		mockName = "local"
	}
	mockID := strings.TrimSpace(cfg.MockID)
	if mockID == "" {
		mockID = "local"
	}

	s := &Service{
		mode:                mode,
		sessionSecret:       secret,
		mockUser:            &User{ID: mockID, Username: mockName, Source: SourceNone},
		discordClientID:     strings.TrimSpace(cfg.DiscordClientID),
		discordClientSecret: strings.TrimSpace(cfg.DiscordClientSecret),
		discordRedirectURL:  strings.TrimSpace(cfg.DiscordRedirectURL),
		discordGuildID:      strings.TrimSpace(cfg.DiscordGuildID),
		frontendURL:         strings.TrimRight(strings.TrimSpace(cfg.FrontendURL), "/"),
		tokens:              newTokenStore(cfg.Store),
		exchanges:           newExchangeStore(),
		logger:              logger,
	}
	if s.frontendURL == "" {
		s.frontendURL = "http://127.0.0.1:5173"
	}

	switch mode {
	case ModeNone, ModeMock:
		// ok
	case ModeDiscord:
		if s.discordClientID == "" || s.discordClientSecret == "" || s.discordRedirectURL == "" {
			return nil, fmt.Errorf("auth-mode=discord requires --discord-client-id, --discord-client-secret, and --discord-redirect-url")
		}
		if s.discordGuildID == "" {
			logger.Warn("auth-mode=discord without --discord-guild-id: ANY Discord account can log in and operate this control plane")
		}
	default:
		return nil, fmt.Errorf("unknown auth mode %q", mode)
	}
	return s, nil
}

// Mode returns the configured auth mode.
func (s *Service) Mode() Mode { return s.mode }

// MockUser returns the local/mock principal (ModeNone / mock-login).
func (s *Service) MockUser() *User {
	u := *s.mockUser
	if s.mode == ModeMock {
		u.Source = SourceMock
	}
	return &u
}

// LoadTokens loads active API tokens from the backing store into the cache.
// Fail-fast: a load error should abort control-plane startup.
func (s *Service) LoadTokens(ctx context.Context) error {
	return s.tokens.load(ctx)
}

// FlushTokens forces a flush of pending LastUsed timestamps.
func (s *Service) FlushTokens(ctx context.Context) error {
	return s.tokens.flush(ctx)
}

// RunTokenFlusher periodically flushes LastUsed until ctx is cancelled.
func (s *Service) RunTokenFlusher(ctx context.Context, interval time.Duration) {
	s.tokens.runFlusher(ctx, interval)
}
