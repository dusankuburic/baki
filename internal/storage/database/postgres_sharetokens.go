package database

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	storageif "pad-analyzer/internal/storage/interfaces"
)

func (b *PostgresStorageBackend) CreateShareToken(ctx context.Context, t *storageif.ShareToken) error {
	if t.ID == "" {
		t.ID = uuid.NewString()
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now().UTC()
	}
	_, err := b.query(ctx).ExecContext(ctx,
		`INSERT INTO share_tokens (id, flow_id, token_hash, created_by, created_at, expires_at)
		 VALUES ($1, $2, $3, $4, $5, $6)`,
		t.ID, t.FlowID, t.TokenHash, t.CreatedBy, t.CreatedAt, t.ExpiresAt)
	return err
}

func (b *PostgresStorageBackend) GetShareTokenByHash(ctx context.Context, tokenHash string) (*storageif.ShareToken, error) {
	t := &storageif.ShareToken{}
	err := b.query(ctx).QueryRowContext(ctx,
		`SELECT id, flow_id, token_hash, created_by, created_at, expires_at
		 FROM share_tokens WHERE token_hash = $1`, tokenHash).
		Scan(&t.ID, &t.FlowID, &t.TokenHash, &t.CreatedBy, &t.CreatedAt, &t.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, storageif.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	// Check expiry
	if t.ExpiresAt != nil && t.ExpiresAt.Before(time.Now()) {
		return nil, storageif.ErrNotFound
	}
	return t, nil
}

func (b *PostgresStorageBackend) ListShareTokens(ctx context.Context, flowID string) ([]*storageif.ShareToken, error) {
	rows, err := b.query(ctx).QueryContext(ctx,
		`SELECT id, flow_id, token_hash, created_by, created_at, expires_at
		 FROM share_tokens
		 WHERE flow_id = $1 AND (expires_at IS NULL OR expires_at > NOW())
		 ORDER BY created_at DESC`, flowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*storageif.ShareToken
	for rows.Next() {
		t := &storageif.ShareToken{}
		if err := rows.Scan(&t.ID, &t.FlowID, &t.TokenHash, &t.CreatedBy, &t.CreatedAt, &t.ExpiresAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

func (b *PostgresStorageBackend) RevokeShareToken(ctx context.Context, flowID, tokenID string) error {
	_, err := b.query(ctx).ExecContext(ctx,
		`DELETE FROM share_tokens WHERE flow_id = $1 AND id = $2`, flowID, tokenID)
	return err
}
