package auth

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
)

const (
	sessionCookieName = "strategon_session"
	sessionTTL        = 7 * 24 * time.Hour
	stateCookieName   = "strategon_oauth_state"
	stateTTL          = 10 * time.Minute
)

type sessionPayload struct {
	ID       string `json:"id"`
	Username string `json:"username"`
	Source   Source `json:"source"`
	Exp      int64  `json:"exp"`
}

func (s *Service) sign(payload []byte) string {
	mac := hmac.New(sha256.New, s.sessionSecret)
	mac.Write(payload)
	sig := mac.Sum(nil)
	return base64.RawURLEncoding.EncodeToString(payload) + "." + base64.RawURLEncoding.EncodeToString(sig)
}

func (s *Service) verify(token string) ([]byte, error) {
	parts := strings.Split(token, ".")
	if len(parts) != 2 {
		return nil, errors.New("malformed token")
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil {
		return nil, err
	}
	sig, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, s.sessionSecret)
	mac.Write(payload)
	if !hmac.Equal(sig, mac.Sum(nil)) {
		return nil, errors.New("bad signature")
	}
	return payload, nil
}

func (s *Service) sessionToken(u *User) (string, error) {
	body, err := json.Marshal(sessionPayload{
		ID:       u.ID,
		Username: u.Username,
		Source:   u.Source,
		Exp:      time.Now().Add(sessionTTL).Unix(),
	})
	if err != nil {
		return "", err
	}
	return s.sign(body), nil
}

func (s *Service) issueSessionCookie(w http.ResponseWriter, u *User, secure bool) error {
	tok, err := s.sessionToken(u)
	if err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    tok,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   int(sessionTTL.Seconds()),
	})
	return nil
}

func (s *Service) clearSessionCookie(w http.ResponseWriter, secure bool) {
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    "",
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   -1,
	})
}

func (s *Service) userFromSessionCookie(r *http.Request) (*User, error) {
	c, err := r.Cookie(sessionCookieName)
	if err != nil || c.Value == "" {
		return nil, err
	}
	return s.userFromSessionValue(c.Value)
}

func (s *Service) issueOAuthState(w http.ResponseWriter, secure bool) (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	state := base64.RawURLEncoding.EncodeToString(b)
	body, err := json.Marshal(map[string]any{"state": state, "exp": time.Now().Add(stateTTL).Unix()})
	if err != nil {
		return "", err
	}
	http.SetCookie(w, &http.Cookie{
		Name:     stateCookieName,
		Value:    s.sign(body),
		Path:     "/auth",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Secure:   secure,
		MaxAge:   int(stateTTL.Seconds()),
	})
	return state, nil
}

func (s *Service) consumeOAuthState(w http.ResponseWriter, r *http.Request, got string, secure bool) error {
	c, err := r.Cookie(stateCookieName)
	if err != nil {
		return errors.New("missing oauth state cookie")
	}
	raw, err := s.verify(c.Value)
	if err != nil {
		return err
	}
	var p struct {
		State string `json:"state"`
		Exp   int64  `json:"exp"`
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return err
	}
	http.SetCookie(w, &http.Cookie{
		Name: stateCookieName, Value: "", Path: "/auth", MaxAge: -1,
		HttpOnly: true, SameSite: http.SameSiteLaxMode, Secure: secure,
	})
	if p.Exp < time.Now().Unix() {
		return errors.New("oauth state expired")
	}
	if p.State == "" || p.State != got {
		return fmt.Errorf("oauth state mismatch")
	}
	return nil
}

func randomSecret() ([]byte, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return nil, err
	}
	return b, nil
}
