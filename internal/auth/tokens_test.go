package auth

import (
	"context"
	"encoding/json"
	"sync"
	"testing"
	"time"

	pb "github.com/bullionbear/strategon/gen/strategyplatform/v1"
	"github.com/bullionbear/strategon/internal/controlplane/store"
)

func TestTokenCreateLookupRevoke(t *testing.T) {
	mem := store.NewMemory(nil)
	svc, err := New(Config{
		Mode:          ModeNone,
		SessionSecret: "test-secret-at-least-32-bytes-long!!",
		Tokens:        mem,
		MockUser:      "alice",
		MockID:        "u1",
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if err := svc.LoadTokens(ctx); err != nil {
		t.Fatal(err)
	}

	owner := &User{ID: "u1", Username: "alice", Source: SourceDiscord}
	plaintext, meta, err := svc.tokens.create(ctx, owner, "ci")
	if err != nil {
		t.Fatal(err)
	}
	if meta.Name != "ci" || meta.UserID != "u1" {
		t.Fatalf("meta %#v", meta)
	}
	if plaintext == "" || meta.ID == "" {
		t.Fatal("empty token")
	}
	if meta.LastUsed != nil {
		t.Fatalf("new token should omit last_used, got %v", meta.LastUsed)
	}

	rows, err := mem.LoadAPITokens(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if rows[0].TokenHash == plaintext || rows[0].TokenHash == "" {
		t.Fatalf("expected hash-only persistence, got %#v", rows[0])
	}

	u, err := svc.tokens.lookup(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != "u1" || u.Source != SourceAPIToken || u.TokenID != meta.ID {
		t.Fatalf("lookup user %#v", u)
	}

	ok, err := svc.tokens.revoke(ctx, &User{ID: "other", Username: "bob", Source: SourceDiscord}, meta.ID)
	if err != nil || ok {
		t.Fatalf("cross-user revoke ok=%v err=%v", ok, err)
	}

	ok, err = svc.tokens.revoke(ctx, owner, meta.ID)
	if err != nil || !ok {
		t.Fatalf("revoke ok=%v err=%v", ok, err)
	}
	if _, err := svc.tokens.lookup(plaintext); err == nil {
		t.Fatal("expected lookup failure after revoke")
	}
	rows, err = mem.LoadAPITokens(ctx)
	if err != nil || len(rows) != 0 {
		t.Fatalf("active rows after revoke: %d err=%v", len(rows), err)
	}
	audit := mem.ListAudit("", "")
	var actions []string
	for _, e := range audit {
		actions = append(actions, e.GetAction())
	}
	if !contains(actions, "CreateToken") || !contains(actions, "RevokeToken") {
		t.Fatalf("audit actions %v", actions)
	}
}

func TestTokenPersistsAcrossServiceRestart(t *testing.T) {
	mem := store.NewMemory(nil)
	cfg := Config{
		Mode:          ModeNone,
		SessionSecret: "test-secret-at-least-32-bytes-long!!",
		Tokens:        mem,
		MockUser:      "alice",
		MockID:        "u1",
	}
	svc1, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner := &User{ID: "u1", Username: "alice", Source: SourceDiscord}
	plaintext, meta, err := svc1.tokens.create(ctx, owner, "long-lived")
	if err != nil {
		t.Fatal(err)
	}

	svc2, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc2.LoadTokens(ctx); err != nil {
		t.Fatal(err)
	}
	u, err := svc2.tokens.lookup(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if u.ID != "u1" || u.TokenID != meta.ID {
		t.Fatalf("after restart %#v", u)
	}
	listed := svc2.tokens.list("u1")
	if len(listed) != 1 || listed[0].ID != meta.ID {
		t.Fatalf("list after restart %#v", listed)
	}
}

func TestLookupDoesNotTouchStore(t *testing.T) {
	counting := newCountingPersist()
	svc, err := New(Config{
		Mode:          ModeNone,
		SessionSecret: "test-secret-at-least-32-bytes-long!!",
		Tokens:        counting,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner := &User{ID: "u1", Username: "alice", Source: SourceDiscord}
	plaintext, _, err := svc.tokens.create(ctx, owner, "t")
	if err != nil {
		t.Fatal(err)
	}
	before := counting.calls()
	if _, err := svc.tokens.lookup(plaintext); err != nil {
		t.Fatal(err)
	}
	if after := counting.calls(); after != before {
		t.Fatalf("lookup hit store: before=%d after=%d", before, after)
	}
}

func TestLastUsedFlush(t *testing.T) {
	mem := store.NewMemory(nil)
	svc, err := New(Config{
		Mode:          ModeNone,
		SessionSecret: "test-secret-at-least-32-bytes-long!!",
		Tokens:        mem,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner := &User{ID: "u1", Username: "alice", Source: SourceDiscord}
	plaintext, meta, err := svc.tokens.create(ctx, owner, "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.tokens.lookup(plaintext); err != nil {
		t.Fatal(err)
	}
	if err := svc.FlushTokens(ctx); err != nil {
		t.Fatal(err)
	}
	rows, err := mem.LoadAPITokens(ctx)
	if err != nil || len(rows) != 1 {
		t.Fatalf("rows=%v err=%v", rows, err)
	}
	if rows[0].ID != meta.ID || rows[0].LastUsed.IsZero() {
		t.Fatalf("expected last_used flushed, got %#v", rows[0])
	}
	if err := svc.FlushTokens(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestFlushIndependentOfExpiredContext(t *testing.T) {
	// Guards shutdown fix: final flush must use its own budget, not a drained ctx.
	persist := newCountingPersist()
	svc, err := New(Config{
		Mode:          ModeNone,
		SessionSecret: "test-secret-at-least-32-bytes-long!!",
		Tokens:        persist,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner := &User{ID: "u1", Username: "alice", Source: SourceDiscord}
	plaintext, meta, err := svc.tokens.create(ctx, owner, "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.tokens.lookup(plaintext); err != nil {
		t.Fatal(err)
	}

	expired, cancel := context.WithCancel(context.Background())
	cancel()
	if err := svc.FlushTokens(expired); err == nil {
		t.Fatal("expected flush failure on expired context")
	}
	// Dirty should have been re-queued; a fresh context must still persist it.
	if err := svc.FlushTokens(context.Background()); err != nil {
		t.Fatal(err)
	}
	rows, err := persist.LoadAPITokens(ctx)
	if err != nil || len(rows) != 1 || rows[0].ID != meta.ID || rows[0].LastUsed.IsZero() {
		t.Fatalf("lookup during drain must land in final flush: %+v err=%v", rows, err)
	}
}

func TestRevokeClearsDirty(t *testing.T) {
	counting := newCountingPersist()
	svc, err := New(Config{
		Mode:          ModeNone,
		SessionSecret: "test-secret-at-least-32-bytes-long!!",
		Tokens:        counting,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	owner := &User{ID: "u1", Username: "alice", Source: SourceDiscord}
	plaintext, meta, err := svc.tokens.create(ctx, owner, "t")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.tokens.lookup(plaintext); err != nil {
		t.Fatal(err)
	}
	ok, err := svc.tokens.revoke(ctx, owner, meta.ID)
	if err != nil || !ok {
		t.Fatalf("revoke ok=%v err=%v", ok, err)
	}
	if err := svc.FlushTokens(ctx); err != nil {
		t.Fatal(err)
	}
	if counting.touchCalls != 0 {
		t.Fatalf("revoked token should not be flushed; touchCalls=%d", counting.touchCalls)
	}
}

func TestLastUsedJSONOmittedWhenUnset(t *testing.T) {
	meta := TokenMeta{ID: "x", Name: "n", UserID: "u", Username: "a", CreatedAt: time.Unix(1, 0).UTC()}
	b, err := json.Marshal(meta)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != `{"id":"x","name":"n","user_id":"u","username":"a","created_at":"1970-01-01T00:00:01Z"}` {
		t.Fatalf("unexpected json %s", b)
	}
}

func TestFlusherRace(t *testing.T) {
	mem := store.NewMemory(nil)
	svc, err := New(Config{
		Mode:          ModeNone,
		SessionSecret: "test-secret-at-least-32-bytes-long!!",
		Tokens:        mem,
	})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go svc.RunTokenFlusher(ctx, 5*time.Millisecond)

	owner := &User{ID: "u1", Username: "alice", Source: SourceDiscord}
	var tokens []string
	for i := 0; i < 8; i++ {
		pt, _, err := svc.tokens.create(ctx, owner, "t")
		if err != nil {
			t.Fatal(err)
		}
		tokens = append(tokens, pt)
	}

	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			for j := 0; j < 50; j++ {
				_, _ = svc.tokens.lookup(tokens[i%len(tokens)])
				if j%10 == 0 {
					_ = svc.FlushTokens(context.Background())
				}
			}
		}(i)
	}
	wg.Wait()
	cancel()
	_ = svc.FlushTokens(context.Background())
}

// countingPersist implements TokenPersistence only (no full Store embed).
type countingPersist struct {
	mu          sync.Mutex
	byID        map[string]store.TokenRow
	loadCalls   int
	insertCalls int
	revokeCalls int
	touchCalls  int
	audit       []*pb.AuditEntry
}

func newCountingPersist() *countingPersist {
	return &countingPersist{byID: map[string]store.TokenRow{}}
}

func (c *countingPersist) calls() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.loadCalls + c.insertCalls + c.revokeCalls + c.touchCalls
}

func (c *countingPersist) LoadAPITokens(ctx context.Context) ([]store.TokenRow, error) {
	_ = ctx
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadCalls++
	out := make([]store.TokenRow, 0, len(c.byID))
	for _, row := range c.byID {
		if row.RevokedAt.IsZero() {
			out = append(out, row)
		}
	}
	return out, nil
}

func (c *countingPersist) InsertAPIToken(ctx context.Context, t store.TokenRow) error {
	_ = ctx
	c.mu.Lock()
	defer c.mu.Unlock()
	c.insertCalls++
	c.byID[t.ID] = t
	return nil
}

func (c *countingPersist) RevokeAPIToken(ctx context.Context, userID, id string) (bool, error) {
	_ = ctx
	c.mu.Lock()
	defer c.mu.Unlock()
	c.revokeCalls++
	row, ok := c.byID[id]
	if !ok || row.UserID != userID || !row.RevokedAt.IsZero() {
		return false, nil
	}
	row.RevokedAt = time.Now().UTC()
	c.byID[id] = row
	return true, nil
}

func (c *countingPersist) TouchAPITokens(ctx context.Context, lastUsed map[string]time.Time) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.touchCalls++
	for id, ts := range lastUsed {
		row, ok := c.byID[id]
		if !ok || !row.RevokedAt.IsZero() {
			continue
		}
		row.LastUsed = ts
		c.byID[id] = row
	}
	return nil
}

func (c *countingPersist) AppendAudit(entry *pb.AuditEntry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.audit = append(c.audit, entry)
	return nil
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
