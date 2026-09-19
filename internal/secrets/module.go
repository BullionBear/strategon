package secrets

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"time"
)

const (
	// MaxPlaintext is the PutSecret size cap.
	MaxPlaintext = 64 * 1024
)

var (
	ErrDark      = errors.New("secret management is unavailable")
	ErrNotFound  = errors.New("secret not found")
	ErrBadRef    = errors.New("invalid secret reference")
	ErrOpen      = errors.New("secret unwrap failed")
	ErrTooLarge  = errors.New("secret value exceeds 64KiB")
	ErrNoPersist = errors.New("secret store is not configured")
)

// View is the human-safe catalog row. Never includes plaintext or ciphertext.
type View struct {
	Name        string
	Token       string
	LengthBytes int32
	KeyID       string
}

// Module is SecretManagement: named rows, wrap key, encrypt-only API.
type Module struct {
	persist Persistence
	key     []byte
	keyID   string
	prevKey []byte
	prevID  string
}

// New constructs a Module. persist may be nil only when cfg.Key is empty
// (dark). A live key without persistence is rejected.
func New(cfg Config, persist Persistence) (*Module, error) {
	if len(cfg.Key) == 0 {
		return &Module{}, nil
	}
	if persist == nil {
		return nil, fmt.Errorf("secrets: %w", ErrNoPersist)
	}
	if _, err := newGCM(cfg.Key); err != nil {
		return nil, err
	}
	id := cfg.KeyID
	if id == "" {
		id = defaultKeyID(cfg.Key)
	}
	if len(cfg.PrevKey) > 0 {
		if _, err := newGCM(cfg.PrevKey); err != nil {
			return nil, fmt.Errorf("secrets: previous wrap key: %w", err)
		}
	}
	return &Module{
		persist: persist,
		key:     append([]byte(nil), cfg.Key...),
		keyID:   id,
		prevKey: append([]byte(nil), cfg.PrevKey...),
		prevID:  cfg.PrevID,
	}, nil
}

// Dark is true when the wrap key is missing. Secret RPCs and resolve fail;
// the rest of the control plane keeps serving.
func (m *Module) Dark() bool {
	return m == nil || len(m.key) == 0
}

// Put encrypts plaintext and upserts the named row. Returns the public token.
func (m *Module) Put(ctx context.Context, name, plaintext string) (string, View, error) {
	if m.Dark() {
		return "", View{}, ErrDark
	}
	if err := ValidateName(name); err != nil {
		return "", View{}, err
	}
	if len(plaintext) > MaxPlaintext {
		return "", View{}, ErrTooLarge
	}
	ct, err := seal(m.key, []byte(plaintext))
	if err != nil {
		return "", View{}, err
	}
	row := Row{
		Name:        name,
		Ciphertext:  ct,
		KeyID:       m.keyID,
		LengthBytes: int32(len(plaintext)),
		UpdatedAt:   time.Now().UTC(),
	}
	if err := m.persist.UpsertSecret(ctx, row); err != nil {
		return "", View{}, err
	}
	return Token(name), row.View(), nil
}

// Get returns catalog metadata. Never unwraps.
func (m *Module) Get(ctx context.Context, name string) (View, error) {
	if m.Dark() {
		return View{}, ErrDark
	}
	if err := ValidateName(name); err != nil {
		return View{}, err
	}
	row, ok, err := m.persist.GetSecret(ctx, name)
	if err != nil {
		return View{}, err
	}
	if !ok {
		return View{}, fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return row.View(), nil
}

// List returns catalog rows, name-sorted.
func (m *Module) List(ctx context.Context) ([]View, error) {
	if m.Dark() {
		return nil, ErrDark
	}
	rows, err := m.persist.ListSecrets(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]View, 0, len(rows))
	for _, row := range rows {
		out = append(out, row.View())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}

// Delete removes the named row. Missing is ErrNotFound. Assignments that
// still reference secret.<name> fail closed on the next southbound resolve.
func (m *Module) Delete(ctx context.Context, name string) error {
	if m.Dark() {
		return ErrDark
	}
	if err := ValidateName(name); err != nil {
		return err
	}
	ok, err := m.persist.DeleteSecret(ctx, name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return nil
}

// Exists reports whether name is in the store. Used at apply time (metadata
// only; does not need to unwrap). Dark and missing are errors.
func (m *Module) Exists(ctx context.Context, name string) error {
	if m.Dark() {
		return ErrDark
	}
	if err := ValidateName(name); err != nil {
		return fmt.Errorf("%w: %s", ErrBadRef, err)
	}
	_, ok, err := m.persist.GetSecret(ctx, name)
	if err != nil {
		return err
	}
	if !ok {
		return fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	return nil
}

// ResolveMap copies env, replacing secret.<name> values with plaintext.
// Ordinary text is copied through. Any ref failure fails the whole map
// (caller must not publish a mixed snapshot).
func (m *Module) ResolveMap(ctx context.Context, env map[string]string) (map[string]string, error) {
	if len(env) == 0 {
		return env, nil
	}
	out := make(map[string]string, len(env))
	for k, v := range env {
		plain, err := m.ResolveValue(ctx, v)
		if err != nil {
			return nil, fmt.Errorf("env %q: %w", k, err)
		}
		out[k] = plain
	}
	return out, nil
}

// ResolveValue unwraps a secret token or returns ordinary text unchanged.
func (m *Module) ResolveValue(ctx context.Context, value string) (string, error) {
	name, isRef, err := ParseRef(value)
	if err != nil {
		return "", err
	}
	if !isRef {
		return value, nil
	}
	if m.Dark() {
		return "", ErrDark
	}
	row, ok, err := m.persist.GetSecret(ctx, name)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", fmt.Errorf("%w: %s", ErrNotFound, name)
	}
	plain, err := m.openRow(row)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func (m *Module) openRow(row Row) ([]byte, error) {
	if row.KeyID == m.keyID || row.KeyID == "" {
		if plain, err := open(m.key, row.Ciphertext); err == nil {
			return plain, nil
		} else if row.KeyID == m.keyID || len(m.prevKey) == 0 {
			return nil, err
		}
	}
	if len(m.prevKey) > 0 && (row.KeyID == m.prevID || row.KeyID == "") {
		return open(m.prevKey, row.Ciphertext)
	}
	// Key id unknown or current failed: try previous as last resort.
	if len(m.prevKey) > 0 {
		if plain, err := open(m.prevKey, row.Ciphertext); err == nil {
			return plain, nil
		}
	}
	return nil, fmt.Errorf("%w: unknown key id %q", ErrOpen, row.KeyID)
}
