package auth

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bullionbear/strategon/internal/controlplane/store"
)

func TestTokenCreateLookupRevoke(t *testing.T) {
	mem := store.NewMemory(nil)
	svc, err := New(Config{
		Mode:          ModeNone,
		SessionSecret: "test-secret-at-least-32-bytes-long!!",
		Store:         mem,
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

	// Persist store holds hash only.
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

	// Cross-user revoke must fail.
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
		Store:         mem,
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
	counting := &countingStore{Memory: store.NewMemory(nil)}
	svc, err := New(Config{
		Mode:          ModeNone,
		SessionSecret: "test-secret-at-least-32-bytes-long!!",
		Store:         counting,
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
	before := counting.loadCalls + counting.insertCalls + counting.revokeCalls + counting.touchCalls
	if _, err := svc.tokens.lookup(plaintext); err != nil {
		t.Fatal(err)
	}
	after := counting.loadCalls + counting.insertCalls + counting.revokeCalls + counting.touchCalls
	if after != before {
		t.Fatalf("lookup hit store: before=%d after=%d", before, after)
	}
}

func TestLastUsedFlush(t *testing.T) {
	mem := store.NewMemory(nil)
	svc, err := New(Config{
		Mode:          ModeNone,
		SessionSecret: "test-secret-at-least-32-bytes-long!!",
		Store:         mem,
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
	// Second flush with empty dirty set is a no-op.
	if err := svc.FlushTokens(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestFlusherRace(t *testing.T) {
	mem := store.NewMemory(nil)
	svc, err := New(Config{
		Mode:          ModeNone,
		SessionSecret: "test-secret-at-least-32-bytes-long!!",
		Store:         mem,
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

type countingStore struct {
	*store.Memory
	mu           sync.Mutex
	loadCalls    int
	insertCalls  int
	revokeCalls  int
	touchCalls   int
}

func (c *countingStore) LoadAPITokens(ctx context.Context) ([]store.TokenRow, error) {
	c.mu.Lock()
	c.loadCalls++
	c.mu.Unlock()
	return c.Memory.LoadAPITokens(ctx)
}

func (c *countingStore) InsertAPIToken(ctx context.Context, t store.TokenRow) error {
	c.mu.Lock()
	c.insertCalls++
	c.mu.Unlock()
	return c.Memory.InsertAPIToken(ctx, t)
}

func (c *countingStore) RevokeAPIToken(ctx context.Context, userID, id string) (bool, error) {
	c.mu.Lock()
	c.revokeCalls++
	c.mu.Unlock()
	return c.Memory.RevokeAPIToken(ctx, userID, id)
}

func (c *countingStore) TouchAPITokens(ctx context.Context, lastUsed map[string]time.Time) error {
	c.mu.Lock()
	c.touchCalls++
	c.mu.Unlock()
	return c.Memory.TouchAPITokens(ctx, lastUsed)
}

func contains(ss []string, want string) bool {
	for _, s := range ss {
		if s == want {
			return true
		}
	}
	return false
}
