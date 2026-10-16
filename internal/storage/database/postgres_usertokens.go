package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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

// UserTokenValid is ConsumeUserToken's match, read only.
func (b *PostgresStorageBackend) UserTokenValid(ctx context.Context, purpose, tokenHash string) (bool, error) {
	var ok bool
	err := b.query(ctx).QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM user_tokens
		  WHERE token_hash = $1 AND purpose = $2 AND used_at IS NULL AND expires_at > $3)`,
		tokenHash, purpose, time.Now().UTC()).Scan(&ok)
	return ok, err
}

// RedeemPasswordReset claims the token and writes the password in a dedicated transaction, taking the users row
// first (FOR NO KEY UPDATE) in the same order the account erasure's DELETE FROM users and its user_tokens cascade
// locks them. The token's UPDATE holds its row until the transaction commits, so an InvalidateUserTokens (an SSO
// adoption's, before it clears the password) either ran first and leaves no token, or waits and finds it used.
func (b *PostgresStorageBackend) RedeemPasswordReset(ctx context.Context, tokenHash, passwordHash string) (string, error) {
	var userID string
	now := time.Now().UTC()
	err := b.withOwnTx(ctx, sql.LevelDefault, func(txCtx context.Context, tx *sql.Tx) error {
		// 1. Lock the users row first (same order as DELETE FROM users -> ON DELETE CASCADE user_tokens).
		err := tx.QueryRowContext(txCtx, `
			SELECT id FROM users
			 WHERE id = (
				SELECT user_id FROM user_tokens
				 WHERE token_hash = $1 AND purpose = $2 AND used_at IS NULL AND expires_at > $3
			 )
			FOR NO KEY UPDATE`,
			tokenHash, interfaces.TokenPurposePasswordReset, now).Scan(&userID)
		if errors.Is(err, sql.ErrNoRows) {
			return interfaces.ErrNotFound
		}
		if err != nil {
			return err
		}

		// 2. Consume the token row.
		var claimedID string
		err = tx.QueryRowContext(txCtx, `
			UPDATE user_tokens SET used_at = $1
			 WHERE token_hash = $2 AND purpose = $3 AND used_at IS NULL AND expires_at > $1
			RETURNING user_id`,
			now, tokenHash, interfaces.TokenPurposePasswordReset).Scan(&claimedID)
		if errors.Is(err, sql.ErrNoRows) {
			return interfaces.ErrNotFound
		}
		if err != nil {
			return err
		}

		// 3. Update the password and clear lockouts.
		_, err = tx.ExecContext(txCtx, `
			UPDATE users SET password = $1, failed_login_attempts = 0, locked_until = NULL, updated_at = NOW()
			 WHERE id = $2`,
			passwordHash, userID)
		return err
	})
	if errors.Is(err, interfaces.ErrNotFound) {
		return "", interfaces.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("redeem password reset: %w", err)
	}
	return userID, nil
}

func (b *PostgresStorageBackend) SetUserEmailVerified(ctx context.Context, userID string) error {
	_, err := b.query(ctx).ExecContext(ctx,
		`UPDATE users SET email_verified = TRUE WHERE id = $1`, userID)
	return err
}

// InvalidateUserTokens stamps used_at on every unused, unexpired token of the
// given purposes owned by userID. Called from the password-change and
// password-reset handlers so all other outstanding reset / verify links for the
// account become unredeemable immediately (defense against leaked-link account
// takeover). Safe to call with zero purposes (no-op).
func (b *PostgresStorageBackend) InvalidateUserTokens(ctx context.Context, userID string, purposes ...string) error {
	if len(purposes) == 0 {
		return nil
	}
	_, err := b.query(ctx).ExecContext(ctx,
		`UPDATE user_tokens
		    SET used_at = $1
		  WHERE user_id = $2
		    AND purpose = ANY($3)
		    AND used_at IS NULL`,
		time.Now().UTC(), userID, purposes)
	return err
}
