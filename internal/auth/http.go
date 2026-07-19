package auth

import (
	"encoding/json"
	"net/http"
	"strings"
)

// Mount registers HTTP routes under /auth/ on mux.
func (s *Service) Mount(mux *http.ServeMux) {
	mux.HandleFunc("/auth/status", s.handleStatus)
	mux.HandleFunc("/auth/me", s.handleMe)
	mux.HandleFunc("/auth/login", s.handleLogin)
	mux.HandleFunc("/auth/callback", s.handleCallback)
	mux.HandleFunc("/auth/logout", s.handleLogout)
	mux.HandleFunc("/auth/mock-login", s.handleMockLogin)
	mux.HandleFunc("/auth/exchange", s.handleExchange)
	mux.HandleFunc("/auth/tokens", s.handleTokens)
	mux.HandleFunc("/auth/tokens/", s.handleTokenByID)
}

func (s *Service) handleStatus(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	u, _ := s.resolveHTTPUser(r)
	writeJSON(w, http.StatusOK, map[string]any{
		"mode": string(s.mode),
		"user": userJSON(u),
	})
}

func (s *Service) handleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	u, err := s.requireHTTPUser(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	writeJSON(w, http.StatusOK, userJSON(u))
}

func (s *Service) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	secure := r.TLS != nil
	switch s.mode {
	case ModeNone:
		http.Redirect(w, r, s.frontendURL, http.StatusFound)
	case ModeMock:
		http.Redirect(w, r, "/auth/mock-login", http.StatusFound)
	case ModeDiscord:
		state, err := s.issueOAuthState(w, secure)
		if err != nil {
			http.Error(w, "failed to start oauth", http.StatusInternalServerError)
			return
		}
		http.Redirect(w, r, s.discordAuthURL(state), http.StatusFound)
	default:
		http.Error(w, "auth not configured", http.StatusInternalServerError)
	}
}

func (s *Service) handleCallback(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.mode != ModeDiscord {
		http.Error(w, "discord auth disabled", http.StatusBadRequest)
		return
	}
	secure := r.TLS != nil
	if errMsg := r.URL.Query().Get("error"); errMsg != "" {
		http.Error(w, "discord oauth error: "+errMsg, http.StatusBadRequest)
		return
	}
	code := r.URL.Query().Get("code")
	state := r.URL.Query().Get("state")
	if code == "" || state == "" {
		http.Error(w, "missing code/state", http.StatusBadRequest)
		return
	}
	if err := s.consumeOAuthState(w, r, state, secure); err != nil {
		http.Error(w, "invalid oauth state", http.StatusBadRequest)
		return
	}
	u, err := s.exchangeDiscordCode(code)
	if err != nil {
		s.logger.Error("discord oauth exchange failed", "err", err)
		http.Error(w, "discord login failed", http.StatusBadGateway)
		return
	}
	if err := s.finishBrowserLogin(w, r, u); err != nil {
		http.Error(w, "session issue failed", http.StatusInternalServerError)
		return
	}
	s.logger.Info("discord login", "user_id", u.ID, "username", u.Username)
}

func (s *Service) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost && r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	s.clearSessionCookie(w, r.TLS != nil)
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
		return
	}
	http.Redirect(w, r, s.frontendURL, http.StatusFound)
}

func (s *Service) handleMockLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.mode != ModeMock && s.mode != ModeNone {
		http.Error(w, "mock login disabled", http.StatusBadRequest)
		return
	}
	u := s.MockUser()
	u.Source = SourceMock
	if name := strings.TrimSpace(r.URL.Query().Get("username")); name != "" {
		u.Username = name
	}
	if id := strings.TrimSpace(r.URL.Query().Get("id")); id != "" {
		u.ID = id
	}
	s.logger.Info("mock login", "user_id", u.ID, "username", u.Username)
	if wantsJSON(r) || r.Method == http.MethodPost {
		if err := s.issueSessionCookie(w, u, r.TLS != nil); err != nil {
			http.Error(w, "session issue failed", http.StatusInternalServerError)
			return
		}
		tok, err := s.sessionToken(u)
		if err != nil {
			http.Error(w, "session issue failed", http.StatusInternalServerError)
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"user":         userJSON(u),
			"access_token": tok,
			"token_type":   "Bearer",
			"expires_in":   int(sessionTTL.Seconds()),
		})
		return
	}
	if err := s.finishBrowserLogin(w, r, u); err != nil {
		http.Error(w, "session issue failed", http.StatusInternalServerError)
		return
	}
}

func (s *Service) finishBrowserLogin(w http.ResponseWriter, r *http.Request, u *User) error {
	if err := s.issueSessionCookie(w, u, r.TLS != nil); err != nil {
		return err
	}
	code, err := s.exchanges.put(u)
	if err != nil {
		return err
	}
	// Cross-origin UIs (Vite :5173 → API :8081) cannot read the HttpOnly cookie;
	// the one-time exchange code bootstraps a Bearer access_token in the SPA.
	http.Redirect(w, r, s.frontendURL+"/#auth_exchange="+code, http.StatusFound)
	return nil
}

func (s *Service) handleExchange(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var body struct {
		Code string `json:"code"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || strings.TrimSpace(body.Code) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "code required"})
		return
	}
	u, ok := s.exchanges.take(strings.TrimSpace(body.Code))
	if !ok {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid or expired code"})
		return
	}
	tok, err := s.sessionToken(u)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "token issue failed"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"user":         userJSON(u),
		"access_token": tok,
		"token_type":   "Bearer",
		"expires_in":   int(sessionTTL.Seconds()),
	})
}

func (s *Service) handleTokens(w http.ResponseWriter, r *http.Request) {
	u, err := s.requireHTTPUser(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	switch r.Method {
	case http.MethodGet:
		writeJSON(w, http.StatusOK, map[string]any{"tokens": s.tokens.list(u.ID)})
	case http.MethodPost:
		var body struct {
			Name string `json:"name"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		// API tokens are owned by the Discord/mock identity, not the token source.
		owner := &User{ID: u.ID, Username: u.Username, Source: u.Source}
		if owner.Source == SourceAPIToken {
			owner.Source = SourceDiscord
		}
		plaintext, meta, err := s.tokens.create(owner, body.Name)
		if err != nil {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
			return
		}
		s.logger.Info("api token created", "user_id", u.ID, "token_id", meta.ID, "name", meta.Name)
		writeJSON(w, http.StatusCreated, map[string]any{
			"token":    plaintext, // shown once
			"metadata": meta,
		})
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

func (s *Service) handleTokenByID(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	u, err := s.requireHTTPUser(r)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "authentication required"})
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/auth/tokens/")
	id = strings.Trim(id, "/")
	if id == "" {
		http.Error(w, "missing token id", http.StatusBadRequest)
		return
	}
	if !s.tokens.revoke(u.ID, id) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "token not found"})
		return
	}
	s.logger.Info("api token revoked", "user_id", u.ID, "token_id", id)
	writeJSON(w, http.StatusOK, map[string]string{"ok": "true"})
}

func (s *Service) resolveHTTPUser(r *http.Request) (*User, error) {
	if s.mode == ModeNone {
		return s.MockUser(), nil
	}
	if u, err := s.userFromBearer(r.Header.Get("Authorization")); err == nil {
		return u, nil
	}
	return s.userFromSessionCookie(r)
}

func (s *Service) requireHTTPUser(r *http.Request) (*User, error) {
	if s.mode == ModeNone {
		return s.MockUser(), nil
	}
	return s.resolveHTTPUser(r)
}

func userJSON(u *User) any {
	if u == nil {
		return nil
	}
	return map[string]any{
		"id":       u.ID,
		"username": u.Username,
		"source":   string(u.Source),
		"actor":    u.Actor(),
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func wantsJSON(r *http.Request) bool {
	accept := r.Header.Get("Accept")
	return strings.Contains(accept, "application/json") ||
		r.Header.Get("X-Requested-With") == "XMLHttpRequest"
}
