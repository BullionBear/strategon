package store

import (
	"context"
	"errors"
	"fmt"

	"github.com/bullionbear/strategon/internal/secrets"
	"github.com/jackc/pgx/v5"
)

var _ secrets.Persistence = (*Postgres)(nil)

func (p *Postgres) UpsertSecret(ctx context.Context, row secrets.Row) error {
	if row.Name == "" {
		return fmt.Errorf("upsert secret: name is required")
	}
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = opCtx()
		defer cancel()
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO secrets (name, ciphertext, key_id, length_bytes, updated_at)
		VALUES ($1,$2,$3,$4,$5)
		ON CONFLICT (name) DO UPDATE SET
			ciphertext=EXCLUDED.ciphertext,
			key_id=EXCLUDED.key_id,
			length_bytes=EXCLUDED.length_bytes,
			updated_at=EXCLUDED.updated_at`,
		row.Name, row.Ciphertext, row.KeyID, row.LengthBytes, row.UpdatedAt.UTC())
	return err
}

func (p *Postgres) GetSecret(ctx context.Context, name string) (secrets.Row, bool, error) {
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = opCtx()
		defer cancel()
	}
	var row secrets.Row
	err := p.pool.QueryRow(ctx, `SELECT name, ciphertext, key_id, length_bytes, updated_at
		FROM secrets WHERE name=$1`, name).Scan(
		&row.Name, &row.Ciphertext, &row.KeyID, &row.LengthBytes, &row.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return secrets.Row{}, false, nil
	}
	if err != nil {
		return secrets.Row{}, false, err
	}
	row.UpdatedAt = row.UpdatedAt.UTC()
	return row, true, nil
}

func (p *Postgres) ListSecrets(ctx context.Context) ([]secrets.Row, error) {
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = opCtx()
		defer cancel()
	}
	rows, err := p.pool.Query(ctx, `SELECT name, ciphertext, key_id, length_bytes, updated_at
		FROM secrets ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []secrets.Row
	for rows.Next() {
		var row secrets.Row
		if err := rows.Scan(&row.Name, &row.Ciphertext, &row.KeyID, &row.LengthBytes, &row.UpdatedAt); err != nil {
			return nil, err
		}
		row.UpdatedAt = row.UpdatedAt.UTC()
		out = append(out, row)
	}
	return out, rows.Err()
}

func (p *Postgres) DeleteSecret(ctx context.Context, name string) (bool, error) {
	if ctx == nil {
		var cancel context.CancelFunc
		ctx, cancel = opCtx()
		defer cancel()
	}
	tag, err := p.pool.Exec(ctx, `DELETE FROM secrets WHERE name=$1`, name)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}
