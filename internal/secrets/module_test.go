package secrets

import (
	"bytes"
	"context"
	"encoding/base64"
	"strings"
	"sync"
	"testing"
)

type memPersist struct {
	mu   sync.Mutex
	rows map[string]Row
}

func (m *memPersist) UpsertSecret(_ context.Context, row Row) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[string]Row{}
	}
	cp := row
	cp.Ciphertext = append([]byte(nil), row.Ciphertext...)
	m.rows[row.Name] = cp
	return nil
}

func (m *memPersist) GetSecret(_ context.Context, name string) (Row, bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	row, ok := m.rows[name]
	if !ok {
		return Row{}, false, nil
	}
	cp := row
	cp.Ciphertext = append([]byte(nil), row.Ciphertext...)
	return cp, true, nil
}

func (m *memPersist) ListSecrets(context.Context) ([]Row, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Row, 0, len(m.rows))
	for _, row := range m.rows {
		cp := row
		cp.Ciphertext = append([]byte(nil), row.Ciphertext...)
		out = append(out, cp)
	}
	return out, nil
}

func (m *memPersist) DeleteSecret(_ context.Context, name string) (bool, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.rows[name]; !ok {
		return false, nil
	}
	delete(m.rows, name)
	return true, nil
}

func testKey() []byte { return bytes.Repeat([]byte{0x11}, 32) }

func testModule(t *testing.T) (*Module, *memPersist) {
	t.Helper()
	p := &memPersist{}
	m, err := New(Config{Key: testKey(), KeyID: "k1"}, p)
	if err != nil {
		t.Fatal(err)
	}
	return m, p
}

func TestParseRef(t *testing.T) {
	name, isRef, err := ParseRef("plain")
	if err != nil || isRef || name != "" {
		t.Fatalf("plain: name=%q isRef=%v err=%v", name, isRef, err)
	}
	name, isRef, err = ParseRef("secret.db-url")
	if err != nil || !isRef || name != "db-url" {
		t.Fatalf("ref: name=%q isRef=%v err=%v", name, isRef, err)
	}
	if _, _, err := ParseRef("secret."); err == nil {
		t.Fatal("empty name should fail")
	}
	if err := ValidateName("secret.db-url"); err == nil {
		t.Fatal("name must not include prefix")
	}
}

func TestPutGetListNeverLeakPayload(t *testing.T) {
	m, p := testModule(t)
	ctx := context.Background()
	token, view, err := m.Put(ctx, "db-url", "postgres://s3cret")
	if err != nil {
		t.Fatal(err)
	}
	if token != "secret.db-url" || view.Token != token || view.LengthBytes != int32(len("postgres://s3cret")) {
		t.Fatalf("token/view = %q %+v", token, view)
	}
	if strings.Contains(view.Name, "postgres") || strings.Contains(view.KeyID, "postgres") {
		t.Fatalf("view leaked: %+v", view)
	}
	got, err := m.Get(ctx, "db-url")
	if err != nil {
		t.Fatal(err)
	}
	if got.Token != token || got.LengthBytes != view.LengthBytes {
		t.Fatalf("get = %+v", got)
	}
	list, err := m.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %#v", err, list)
	}
	row, ok, _ := p.GetSecret(ctx, "db-url")
	if !ok || bytes.Contains(row.Ciphertext, []byte("postgres://s3cret")) {
		t.Fatalf("ciphertext should not contain plaintext: %q", row.Ciphertext)
	}
	plain, err := m.ResolveValue(ctx, token)
	if err != nil || plain != "postgres://s3cret" {
		t.Fatalf("resolve: %q %v", plain, err)
	}
}

func TestPutReSealDoesNotChangeToken(t *testing.T) {
	m, p := testModule(t)
	ctx := context.Background()
	tok1, _, err := m.Put(ctx, "db-url", "v1")
	if err != nil {
		t.Fatal(err)
	}
	first, _, _ := p.GetSecret(ctx, "db-url")
	tok2, _, err := m.Put(ctx, "db-url", "v1")
	if err != nil {
		t.Fatal(err)
	}
	second, _, _ := p.GetSecret(ctx, "db-url")
	if tok1 != tok2 || tok1 != "secret.db-url" {
		t.Fatalf("tokens %q %q", tok1, tok2)
	}
	if bytes.Equal(first.Ciphertext, second.Ciphertext) {
		t.Fatal("re-put must use a new nonce")
	}
}

func TestDelete(t *testing.T) {
	m, _ := testModule(t)
	ctx := context.Background()
	if _, _, err := m.Put(ctx, "db-url", "v1"); err != nil {
		t.Fatal(err)
	}
	if err := m.Delete(ctx, "db-url"); err != nil {
		t.Fatal(err)
	}
	if _, err := m.Get(ctx, "db-url"); err == nil || !strings.Contains(err.Error(), "not found") {
		t.Fatalf("get after delete: %v", err)
	}
	if err := m.Delete(ctx, "db-url"); err == nil {
		t.Fatal("second delete should be not found")
	}
}

func TestDark(t *testing.T) {
	m, err := New(Config{}, nil)
	if err != nil || !m.Dark() {
		t.Fatalf("empty key should be dark: %v dark=%v", err, m.Dark())
	}
	ctx := context.Background()
	if _, _, err := m.Put(ctx, "x", "y"); err != ErrDark {
		t.Fatalf("put dark: %v", err)
	}
	if _, err := m.List(ctx); err != ErrDark {
		t.Fatalf("list dark: %v", err)
	}
	if err := m.Delete(ctx, "x"); err != ErrDark {
		t.Fatalf("delete dark: %v", err)
	}
	if err := ValidateMaps(ctx, m, map[string]string{"A": "plain"}); err != nil {
		t.Fatalf("plain env while dark: %v", err)
	}
	if err := ValidateMaps(ctx, m, map[string]string{"A": "secret.db-url"}); err == nil {
		t.Fatal("ref while dark should fail")
	}
}

func TestValidateMapsUnknown(t *testing.T) {
	m, _ := testModule(t)
	ctx := context.Background()
	if _, _, err := m.Put(ctx, "db-url", "p"); err != nil {
		t.Fatal(err)
	}
	if err := ValidateMaps(ctx, m, map[string]string{"A": "secret.db-url", "B": "plain"}); err != nil {
		t.Fatal(err)
	}
	if err := ValidateMaps(ctx, m, map[string]string{"A": "secret.missing"}); err == nil {
		t.Fatal("unknown name")
	}
}

func TestResolveFailClosed(t *testing.T) {
	m, _ := testModule(t)
	ctx := context.Background()
	if _, _, err := m.Put(ctx, "ok", "plain"); err != nil {
		t.Fatal(err)
	}
	_, err := m.ResolveMap(ctx, map[string]string{"A": "secret.ok", "B": "secret.missing"})
	if err == nil {
		t.Fatal("mixed resolve must fail")
	}
}

func TestConfigFromEnv(t *testing.T) {
	t.Setenv(envKey, base64.StdEncoding.EncodeToString(testKey()))
	t.Setenv(envKeyID, "prod")
	cfg, err := ConfigFromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if cfg.KeyID != "prod" || len(cfg.Key) != 32 {
		t.Fatalf("cfg = %+v", cfg)
	}
	t.Setenv(envKey, "not-a-key")
	if _, err := ConfigFromEnv(); err == nil {
		t.Fatal("garbage key must fail")
	}
}

func TestPrevKeyUnwrap(t *testing.T) {
	oldKey := bytes.Repeat([]byte{0x22}, 32)
	p := &memPersist{}
	old, err := New(Config{Key: oldKey, KeyID: "old"}, p)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, _, err := old.Put(ctx, "db-url", "legacy"); err != nil {
		t.Fatal(err)
	}
	cur, err := New(Config{Key: testKey(), KeyID: "new", PrevKey: oldKey, PrevID: "old"}, p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := cur.ResolveValue(ctx, "secret.db-url")
	if err != nil || got != "legacy" {
		t.Fatalf("prev unwrap: %q %v", got, err)
	}
}
