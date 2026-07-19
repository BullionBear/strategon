package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

const apiTokenPrefix = "str_live_"

// TokenMeta is safe to return to clients (never includes the secret).
type TokenMeta struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	UserID    string    `json:"user_id"`
	Username  string    `json:"username"`
	CreatedAt time.Time `json:"created_at"`
	LastUsed  time.Time `json:"last_used,omitempty"`
}

type tokenRecord struct {
	meta    TokenMeta
	hash    string // hex SHA-256 of full token
	prefix8 string // first 8 chars after prefix for Actor()
}

type tokenStore struct {
	mu   sync.RWMutex
	byID map[string]*tokenRecord
}

func newTokenStore() *tokenStore {
	return &tokenStore{byID: make(map[string]*tokenRecord)}
}

// exchangeStore holds one-time codes used to bootstrap a browser Bearer token
// after OAuth when the UI and API are on different origins.
type exchangeStore struct {
	mu   sync.Mutex
	byID map[string]exchangeEntry
}

type exchangeEntry struct {
	user      *User
	expiresAt time.Time
}

func newExchangeStore() *exchangeStore {
	return &exchangeStore{byID: make(map[string]exchangeEntry)}
}

func (e *exchangeStore) put(u *User) (string, error) {
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	code := base64.RawURLEncoding.EncodeToString(b)
	e.mu.Lock()
	e.byID[code] = exchangeEntry{user: u, expiresAt: time.Now().Add(2 * time.Minute)}
	e.mu.Unlock()
	return code, nil
}

func (e *exchangeStore) take(code string) (*User, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	ent, ok := e.byID[code]
	if !ok {
		return nil, false
	}
	delete(e.byID, code)
	if time.Now().After(ent.expiresAt) {
		return nil, false
	}
	return ent.user, true
}

func hashToken(raw string) string {
	sum := sha256.Sum256([]byte(raw))
	return hex.EncodeToString(sum[:])
}

func (t *tokenStore) create(owner *User, name string) (plaintext string, meta TokenMeta, err error) {
	if owner == nil || owner.ID == "" {
		return "", TokenMeta{}, errors.New("owner required")
	}
	name = strings.TrimSpace(name)
	if name == "" {
		name = "default"
	}
	secret := make([]byte, 24)
	if _, err := rand.Read(secret); err != nil {
		return "", TokenMeta{}, err
	}
	idBytes := make([]byte, 8)
	if _, err := rand.Read(idBytes); err != nil {
		return "", TokenMeta{}, err
	}
	id := hex.EncodeToString(idBytes)
	plaintext = apiTokenPrefix + id + "_" + base64.RawURLEncoding.EncodeToString(secret)
	meta = TokenMeta{
		ID:        id,
		Name:      name,
		UserID:    owner.ID,
		Username:  owner.Username,
		CreatedAt: time.Now().UTC(),
	}
	rec := &tokenRecord{
		meta:    meta,
		hash:    hashToken(plaintext),
		prefix8: id,
	}
	t.mu.Lock()
	t.byID[id] = rec
	t.mu.Unlock()
	return plaintext, meta, nil
}

func (t *tokenStore) lookup(raw string) (*User, error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, apiTokenPrefix) {
		return nil, errors.New("not an api token")
	}
	h := hashToken(raw)
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, rec := range t.byID {
		if rec.hash == h {
			rec.meta.LastUsed = time.Now().UTC()
			return &User{
				ID:       rec.meta.UserID,
				Username: rec.meta.Username,
				Source:   SourceAPIToken,
				TokenID:  rec.prefix8,
			}, nil
		}
	}
	return nil, errors.New("unknown api token")
}

func (t *tokenStore) list(userID string) []TokenMeta {
	t.mu.RLock()
	defer t.mu.RUnlock()
	out := make([]TokenMeta, 0)
	for _, rec := range t.byID {
		if rec.meta.UserID == userID {
			out = append(out, rec.meta)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].CreatedAt.After(out[j].CreatedAt)
	})
	return out
}

func (t *tokenStore) revoke(userID, tokenID string) bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ok := t.byID[tokenID]
	if !ok || rec.meta.UserID != userID {
		return false
	}
	delete(t.byID, tokenID)
	return true
}
