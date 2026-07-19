// Package auth provides human-API authentication for ControlPlaneService:
// Discord OAuth (browser sessions), long-lived API tokens, and a local mock
// user. Authorization is flat: any authenticated principal is a full operator.
// Agent/lease mTLS is orthogonal and lives in internal/mtls.
package auth

import (
	"context"
	"fmt"
	"strings"
)

// Mode selects how the human API authenticates callers.
type Mode string

const (
	// ModeNone disables auth checks and injects a mock local principal so audit
	// still records an actor. Default for local/CI.
	ModeNone Mode = "none"
	// ModeMock requires a session (or API token) but issues sessions via
	// /auth/mock-login instead of Discord — local UI/auth-flow testing.
	ModeMock Mode = "mock"
	// ModeDiscord requires Discord OAuth (or a token issued after Discord login).
	ModeDiscord Mode = "discord"
)

// ParseMode accepts none|mock|discord (case-insensitive).
func ParseMode(s string) (Mode, error) {
	switch Mode(strings.ToLower(strings.TrimSpace(s))) {
	case ModeNone, "":
		return ModeNone, nil
	case ModeMock:
		return ModeMock, nil
	case ModeDiscord:
		return ModeDiscord, nil
	default:
		return "", fmt.Errorf("unknown auth mode %q (want none|mock|discord)", s)
	}
}

// Source identifies how the principal was authenticated.
type Source string

const (
	SourceNone     Source = "none"      // injected in ModeNone
	SourceMock     Source = "mock"      // mock-login session
	SourceDiscord  Source = "discord"   // Discord OAuth session
	SourceAPIToken Source = "api_token" // Bearer API token
)

// User is the authenticated human principal.
type User struct {
	ID       string // Discord snowflake, or "local" / mock id
	Username string // Discord username (or mock display name)
	Source   Source
	TokenID  string // set when SourceAPIToken
}

// Actor formats a stable audit-log identity.
func (u *User) Actor() string {
	if u == nil {
		return "anonymous"
	}
	name := u.Username
	if name == "" {
		name = u.ID
	}
	switch u.Source {
	case SourceNone:
		return name
	case SourceMock:
		return fmt.Sprintf("%s (mock)", name)
	case SourceDiscord:
		return fmt.Sprintf("%s (discord:%s)", name, u.ID)
	case SourceAPIToken:
		short := u.TokenID
		if len(short) > 8 {
			short = short[:8]
		}
		return fmt.Sprintf("%s (discord:%s api-token:%s)", name, u.ID, short)
	default:
		return name
	}
}

type ctxKey struct{}

// WithUser attaches u to ctx.
func WithUser(ctx context.Context, u *User) context.Context {
	return context.WithValue(ctx, ctxKey{}, u)
}

// UserFromContext returns the authenticated user, if any.
func UserFromContext(ctx context.Context) (*User, bool) {
	u, ok := ctx.Value(ctxKey{}).(*User)
	return u, ok && u != nil
}

// ActorFromContext returns the audit actor string for the current request.
func ActorFromContext(ctx context.Context) string {
	if u, ok := UserFromContext(ctx); ok {
		return u.Actor()
	}
	return "anonymous"
}
