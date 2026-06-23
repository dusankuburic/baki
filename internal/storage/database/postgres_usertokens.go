package database

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// One-shot user tokens (password reset, email verification). Looked up by hash
// before any user context exists, so there is no RLS — the secret hash is the
// access control (see user_tokens in postgres_migrations.go).

func (b *PostgresStorageBackend) CreateUserToken(ctx context.Context, t *interfaces.UserToken) error {
	_, err := b.query(ctx).ExecContext(ctx,
		`INSERT INTO user_tokens (token_hash, purpose, user_id, expires_at) VALUES ($1, $2, $3, $4)`,
		t.TokenHash, t.Purpose, t.UserID, t.ExpiresAt)
	return err
}

// ConsumeUserToken atomically claims a valid token: the UPDATE only matches an
// unused, unexpired row of the requested purpose and stamps used_at, so a token
// can be redeemed at most once even under concurrent requests. A miss (already
// used, expired, or unknown) returns ErrNotFound.
func (b *PostgresStorageBackend) ConsumeUserToken(ctx context.Context, purpose, tokenHash string) (string, error) {
	var userID string
	err := b.query(ctx).QueryRowContext(ctx,
		`UPDATE user_tokens
		    SET used_at = $1
		  WHERE token_hash = $2 AND purpose = $3 AND used_at IS NULL AND expires_at > $1
		RETURNING user_id`,
		time.Now().UTC(), tokenHash, purpose).Scan(&userID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", interfaces.ErrNotFound
	}
	if err != nil {
		return "", err
	}
	return userID, nil
}

func (b *PostgresStorageBackend) SetUserEmailVerified(ctx context.Context, userID string) error {
	_, err := b.query(ctx).ExecContext(ctx,
		`UPDATE users SET email_verified = TRUE WHERE id = $1`, userID)
	return err
}
