package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"pad-analyzer/internal/storage/interfaces"
)

// joinScopes/splitScopes marshal the scope list through the CSV column —
// scope names are a closed, comma-free set (auth.ValidTokenScopes).
func joinScopes(scopes []string) string { return strings.Join(scopes, ",") }

func splitScopes(csv string) []string {
	if csv == "" {
		return nil
	}
	return strings.Split(csv, ",")
}

// API tokens (machine credentials). Looked up by hash during authentication, so
// there is no RLS — access is constrained by the secret hash and by user_id
// scoping in the list/delete queries (see api_tokens in postgres_migrations.go).

func (b *PostgresStorageBackend) CreateAPIToken(ctx context.Context, t *interfaces.APIToken) error {
	_, err := b.query(ctx).ExecContext(ctx,
		`INSERT INTO api_tokens (id, user_id, name, token_hash, expires_at, scopes) VALUES ($1, $2, $3, $4, $5, $6)`,
		t.ID, t.UserID, t.Name, t.TokenHash, t.ExpiresAt, joinScopes(t.Scopes))
	return err
}

func (b *PostgresStorageBackend) GetAPITokenByHash(ctx context.Context, tokenHash string) (*interfaces.APIToken, error) {
	var t interfaces.APIToken
	var scopes string
	err := b.query(ctx).QueryRowContext(ctx,
		`SELECT id, user_id, name, token_hash, created_at, expires_at, scopes FROM api_tokens WHERE token_hash = $1`, tokenHash).
		Scan(&t.ID, &t.UserID, &t.Name, &t.TokenHash, &t.CreatedAt, &t.ExpiresAt, &scopes)
	t.Scopes = splitScopes(scopes)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, interfaces.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (b *PostgresStorageBackend) ListAPITokens(ctx context.Context, userID string) ([]*interfaces.APIToken, error) {
	rows, err := b.query(ctx).QueryContext(ctx,
		`SELECT id, user_id, name, token_hash, created_at, expires_at, scopes FROM api_tokens WHERE user_id = $1 ORDER BY created_at DESC`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*interfaces.APIToken, 0)
	for rows.Next() {
		var t interfaces.APIToken
		var scopes string
		if err := rows.Scan(&t.ID, &t.UserID, &t.Name, &t.TokenHash, &t.CreatedAt, &t.ExpiresAt, &scopes); err != nil {
			return nil, err
		}
		t.Scopes = splitScopes(scopes)
		out = append(out, &t)
	}
	return out, rows.Err()
}

func (b *PostgresStorageBackend) DeleteAPIToken(ctx context.Context, userID, id string) error {
	_, err := b.query(ctx).ExecContext(ctx,
		`DELETE FROM api_tokens WHERE id = $1 AND user_id = $2`, id, userID)
	return err
}
