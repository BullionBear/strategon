package auth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/controlplane/store"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const apiTokenPrefix = "str_live_"

// DefaultTokenFlushInterval is how often LastUsed dirty entries are flushed.
const DefaultTokenFlushInterval = 30 * time.Second

// TokenMeta is safe to return to clients (never includes the secret).
type TokenMeta struct {
	ID        string     `json:"id"`
	Name      string     `json:"name"`
	UserID    string     `json:"user_id"`
	Username  string     `json:"username"`
	CreatedAt time.Time  `json:"created_at"`
	LastUsed  *time.Time `json:"last_used,omitempty"`
}

type tokenRecord struct {
	meta    TokenMeta
	hash    string // hex SHA-256 of full token
	prefix8 string // first 8 chars after prefix for Actor()
}

type tokenStore struct {
	mu      sync.RWMutex
	byID    map[string]*tokenRecord
	byHash  map[string]*tokenRecord // O(1) lookup by token hash
	dirty   map[string]time.Time    // id -> last_used pending flush
	persist TokenPersistence        // nil = ephemeral cache only (tests)
}

func newTokenStore(persist TokenPersistence) *tokenStore {
	return &tokenStore{
		byID:    make(map[string]*tokenRecord),
		byHash:  make(map[string]*tokenRecord),
		dirty:   make(map[string]time.Time),
		persist: persist,
	}
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

func (t *tokenStore) load(ctx context.Context) error {
	if t.persist == nil {
		return nil
	}
	rows, err := t.persist.LoadAPITokens(ctx)
	if err != nil {
		return err
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	t.byID = make(map[string]*tokenRecord, len(rows))
	t.byHash = make(map[string]*tokenRecord, len(rows))
	t.dirty = make(map[string]time.Time)
	for _, row := range rows {
		rec := recordFromRow(row)
		t.byID[rec.meta.ID] = rec
		t.byHash[rec.hash] = rec
	}
	return nil
}

func recordFromRow(row store.TokenRow) *tokenRecord {
	meta := TokenMeta{
		ID:        row.ID,
		Name:      row.Name,
		UserID:    row.UserID,
		Username:  row.Username,
		CreatedAt: row.CreatedAt.UTC(),
	}
	if !row.LastUsed.IsZero() {
		lu := row.LastUsed.UTC()
		meta.LastUsed = &lu
	}
	return &tokenRecord{
		meta:    meta,
		hash:    row.TokenHash,
		prefix8: row.ID,
	}
}

func (t *tokenStore) create(ctx context.Context, owner *User, name string) (plaintext string, meta TokenMeta, err error) {
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
	hash := hashToken(plaintext)
	if t.persist != nil {
		if err := t.persist.InsertAPIToken(ctx, store.TokenRow{
			ID:        meta.ID,
			TokenHash: hash,
			Name:      meta.Name,
			UserID:    meta.UserID,
			Username:  meta.Username,
			CreatedAt: meta.CreatedAt,
		}); err != nil {
			return "", TokenMeta{}, err
		}
		_ = t.persist.AppendAudit(&pb.AuditEntry{
			Timestamp: timestamppb.Now(),
			Actor:     owner.Actor(),
			Action:    "CreateToken",
			Detail:    fmt.Sprintf("token_id=%s name=%s", meta.ID, meta.Name),
		})
	}
	rec := &tokenRecord{
		meta:    meta,
		hash:    hash,
		prefix8: id,
	}
	t.mu.Lock()
	t.byID[id] = rec
	t.byHash[hash] = rec
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
	rec := t.byHash[h]
	if rec == nil {
		return nil, errors.New("unknown api token")
	}
	now := time.Now().UTC()
	rec.meta.LastUsed = &now
	t.dirty[rec.meta.ID] = now
	return &User{
		ID:       rec.meta.UserID,
		Username: rec.meta.Username,
		Source:   SourceAPIToken,
		TokenID:  rec.prefix8,
	}, nil
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

func (t *tokenStore) revoke(ctx context.Context, actor *User, tokenID string) (bool, error) {
	if actor == nil || actor.ID == "" {
		return false, errors.New("actor required")
	}
	t.mu.RLock()
	rec, ok := t.byID[tokenID]
	owned := ok && rec.meta.UserID == actor.ID
	var tokenName string
	if owned {
		tokenName = rec.meta.Name // Name is immutable after create
	}
	t.mu.RUnlock()
	if !owned {
		return false, nil
	}

	if t.persist != nil {
		ok, err := t.persist.RevokeAPIToken(ctx, actor.ID, tokenID)
		if err != nil {
			return false, err
		}
		if !ok {
			return false, nil
		}
		_ = t.persist.AppendAudit(&pb.AuditEntry{
			Timestamp: timestamppb.Now(),
			Actor:     actor.Actor(),
			Action:    "RevokeToken",
			Detail:    fmt.Sprintf("token_id=%s name=%s", tokenID, tokenName),
		})
	}

	t.mu.Lock()
	defer t.mu.Unlock()
	rec, ok = t.byID[tokenID]
	if !ok || rec.meta.UserID != actor.ID {
		return false, nil
	}
	delete(t.byID, tokenID)
	delete(t.byHash, rec.hash)
	delete(t.dirty, tokenID)
	return true, nil
}

// flush writes dirty LastUsed timestamps to the persist store and clears them.
func (t *tokenStore) flush(ctx context.Context) error {
	t.mu.Lock()
	if len(t.dirty) == 0 {
		t.mu.Unlock()
		return nil
	}
	batch := t.dirty
	t.dirty = make(map[string]time.Time)
	t.mu.Unlock()

	if t.persist == nil {
		return nil
	}
	if err := t.persist.TouchAPITokens(ctx, batch); err != nil {
		t.mu.Lock()
		for id, ts := range batch {
			if existing, ok := t.dirty[id]; !ok || ts.After(existing) {
				t.dirty[id] = ts
			}
		}
		t.mu.Unlock()
		return err
	}
	return nil
}

// runFlusher periodically flushes LastUsed until ctx is cancelled.
func (t *tokenStore) runFlusher(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = DefaultTokenFlushInterval
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			_ = t.flush(ctx)
		}
	}
}
