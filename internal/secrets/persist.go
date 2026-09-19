package secrets

import (
	"context"
	"time"
)

// Row is one persisted Secret. Ciphertext is store-internal.
type Row struct {
	Name        string
	Ciphertext  []byte
	KeyID       string
	LengthBytes int32
	UpdatedAt   time.Time
}

// View is the human-safe projection of a row.
func (r Row) View() View {
	return View{
		Name:        r.Name,
		Token:       Token(r.Name),
		LengthBytes: r.LengthBytes,
		KeyID:       r.KeyID,
	}
}

// Persistence is the narrow store surface SecretManagement needs.
// controlplane/store Memory and Postgres satisfy this.
type Persistence interface {
	UpsertSecret(ctx context.Context, row Row) error
	GetSecret(ctx context.Context, name string) (Row, bool, error)
	ListSecrets(ctx context.Context) ([]Row, error)
	DeleteSecret(ctx context.Context, name string) (bool, error)
}
