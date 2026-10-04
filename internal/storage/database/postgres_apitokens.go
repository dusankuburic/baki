package database

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"pad-analyzer/internal/auth"
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

// GetAPITokenWithOwnerByHash authenticates a PAT in ONE round trip: the token
// row joined to its owner's CURRENT user row (role, email), which PAT
// authentication otherwise fetched with a second sequential query on every
// machine request. The inner join drops a token whose owner is gone, matching
// the old "user lookup failed ⇒ 401" outcome (ErrNotFound either way).
// Deliberately uncached, like the two-query path: revocation (a delete) and a
// role change take effect on the very next request.
func (b *PostgresStorageBackend) GetAPITokenWithOwnerByHash(ctx context.Context, tokenHash string) (*interfaces.APIToken, *interfaces.User, error) {
	var t interfaces.APIToken
	var u interfaces.User
	var scopes, roleStr string
	err := b.query(ctx).QueryRowContext(ctx, `
		SELECT t.id, t.user_id, t.name, t.token_hash, t.created_at, t.expires_at, t.scopes,
		       u.id, u.email, u.role, u.email_verified, u.failed_login_attempts, u.locked_until,
		       u.display_name, u.avatar_url, u.created_at, u.updated_at
		FROM api_tokens t
		JOIN users u ON u.id = t.user_id
		WHERE t.token_hash = $1`, tokenHash).
		Scan(&t.ID, &t.UserID, &t.Name, &t.TokenHash, &t.CreatedAt, &t.ExpiresAt, &scopes,
			&u.ID, &u.Email, &roleStr, &u.EmailVerified, &u.FailedLoginAttempts, &u.LockedUntil,
			&u.DisplayName, &u.AvatarURL, &u.CreatedAt, &u.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, interfaces.ErrNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	t.Scopes = splitScopes(scopes)
	u.Role = auth.Role(roleStr)
	return &t, &u, nil
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
