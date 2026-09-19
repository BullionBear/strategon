package store

import (
	"context"
	"fmt"
	"sort"

	"github.com/bullionbear/strategon/internal/secrets"
)

var _ secrets.Persistence = (*Memory)(nil)

func (m *Memory) UpsertSecret(ctx context.Context, row secrets.Row) error {
	_ = ctx
	if row.Name == "" {
		return fmt.Errorf("upsert secret: name is required")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.secrets == nil {
		m.secrets = map[string]secrets.Row{}
	}
	cp := row
	cp.Ciphertext = append([]byte(nil), row.Ciphertext...)
	m.secrets[row.Name] = cp
	return nil
}

func (m *Memory) GetSecret(ctx context.Context, name string) (secrets.Row, bool, error) {
	_ = ctx
	m.mu.RLock()
	defer m.mu.RUnlock()
	row, ok := m.secrets[name]
	if !ok {
		return secrets.Row{}, false, nil
	}
	cp := row
	cp.Ciphertext = append([]byte(nil), row.Ciphertext...)
	return cp, true, nil
}

func (m *Memory) ListSecrets(ctx context.Context) ([]secrets.Row, error) {
	_ = ctx
	m.mu.RLock()
	defer m.mu.RUnlock()
	out := make([]secrets.Row, 0, len(m.secrets))
	for _, row := range m.secrets {
		cp := row
		cp.Ciphertext = append([]byte(nil), row.Ciphertext...)
		out = append(out, cp)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out, nil
}
