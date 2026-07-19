package auth

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"connectrpc.com/connect"
)

// HandlerOptions returns Connect handler options that authenticate unary and
// streaming RPCs. ModeNone injects the mock local user; other modes require a
// session cookie or Authorization: Bearer <api-token>.
func (s *Service) HandlerOptions() []connect.HandlerOption {
	return []connect.HandlerOption{connect.WithInterceptors(&streamInterceptor{s: s})}
}

type streamInterceptor struct{ s *Service }

func (i *streamInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		u, err := i.s.authenticate(req.Header().Get("Authorization"), cookieHeader(req))
		if err != nil {
			return nil, err
		}
		return next(WithUser(ctx, u), req)
	}
}

func (i *streamInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *streamInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		cookie := strings.Join(conn.RequestHeader().Values("Cookie"), "; ")
		u, err := i.s.authenticate(conn.RequestHeader().Get("Authorization"), cookie)
		if err != nil {
			return err
		}
		return next(WithUser(ctx, u), conn)
	}
}

func cookieHeader(req connect.AnyRequest) string {
	if v := req.Header().Values("Cookie"); len(v) > 0 {
		return strings.Join(v, "; ")
	}
	return req.Header().Get("Cookie")
}

func (s *Service) authenticate(authorization, cookie string) (*User, error) {
	if s.mode == ModeNone {
		return s.MockUser(), nil
	}

	if strings.TrimSpace(authorization) != "" {
		u, err := s.userFromBearer(authorization)
		if err != nil {
			return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid api token"))
		}
		return u, nil
	}

	if u, err := s.userFromCookieString(cookie); err == nil {
		return u, nil
	}

	return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("authentication required"))
}

func (s *Service) userFromBearer(authorization string) (*User, error) {
	authz := strings.TrimSpace(authorization)
	const prefix = "bearer "
	if len(authz) < len(prefix) || !strings.EqualFold(authz[:len(prefix)], prefix) {
		return nil, errors.New("not bearer")
	}
	raw := strings.TrimSpace(authz[len(prefix):])
	// Long-lived API tokens.
	if u, err := s.tokens.lookup(raw); err == nil {
		return u, nil
	}
	// Browser bootstrap / session bearer (same signed blob as the cookie).
	if u, err := s.userFromSessionValue(raw); err == nil {
		return u, nil
	}
	return nil, errors.New("invalid bearer")
}

func (s *Service) userFromSessionValue(sessionVal string) (*User, error) {
	raw, err := s.verify(sessionVal)
	if err != nil {
		return nil, err
	}
	var p sessionPayload
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if p.Exp < time.Now().Unix() {
		return nil, errors.New("session expired")
	}
	if p.ID == "" {
		return nil, errors.New("empty session user")
	}
	src := p.Source
	if src == "" {
		src = SourceDiscord
	}
	return &User{ID: p.ID, Username: p.Username, Source: src}, nil
}

func (s *Service) userFromCookieString(cookieHeader string) (*User, error) {
	if cookieHeader == "" {
		return nil, errors.New("no cookie")
	}
	var sessionVal string
	for _, part := range strings.Split(cookieHeader, ";") {
		part = strings.TrimSpace(part)
		key, val, ok := strings.Cut(part, "=")
		if ok && key == sessionCookieName {
			sessionVal = val
			break
		}
	}
	if sessionVal == "" {
		return nil, errors.New("no session cookie")
	}
	return s.userFromSessionValue(sessionVal)
}
