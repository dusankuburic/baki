package database

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// SaveIdentityLink inserts an IdP-identity → user mapping. The (provider,
// subject) pair is the primary key; re-linking the same external identity to a
// different user is rejected rather than silently re-pointed.
func (b *PostgresStorageBackend) SaveIdentityLink(ctx context.Context, link *interfaces.IdentityLink) error {
	return saveIdentityLinkOn(ctx, b.db, link)
}

// SaveUserWithIdentityLink writes an SSO sign-up's account and its link in a transaction of its own, like
// CreateUser: on the pool, never the request's, so a request that fails afterwards does not undo a sign-up that
// already answered. The user goes first, for the link's foreign key.
func (b *PostgresStorageBackend) SaveUserWithIdentityLink(ctx context.Context, user *interfaces.User, link *interfaces.IdentityLink) error {
	return b.withOwnTx(ctx, sql.LevelDefault, func(txCtx context.Context, tx *sql.Tx) error {
		if err := adoptOrInsertUserOn(txCtx, tx, user); err != nil {
			return err
		}
		return saveIdentityLinkOn(txCtx, tx, link)
	})
}

// adoptOrInsertUserOn inserts a new user; an existing one (an SSO adoption, from a row read before this write) gets
// only its verification and password, and only while the stored row is unverified: the conflict's WHERE takes the
// row lock, so an owner verifying at the same instant is either before (ErrAccountVerified) or after.
func adoptOrInsertUserOn(ctx context.Context, exec DBTX, user *interfaces.User) error {
	now := time.Now().UTC()
	if user.CreatedAt.IsZero() {
		user.CreatedAt = now
	}
	user.UpdatedAt = now
	res, err := exec.ExecContext(ctx, `
		INSERT INTO users (id, email, password, role, email_verified, failed_login_attempts, locked_until, display_name, avatar_url, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		ON CONFLICT (id) DO UPDATE SET
			email_verified = EXCLUDED.email_verified,
			password       = EXCLUDED.password,
			updated_at     = EXCLUDED.updated_at
		WHERE NOT users.email_verified`,
		user.ID, user.Email, user.Password, string(user.Role), user.EmailVerified, user.FailedLoginAttempts, user.LockedUntil, user.DisplayName, user.AvatarURL, user.CreatedAt, user.UpdatedAt,
	)
	if err != nil {
		if isPgErrCode(err, pgErrUniqueViolation) {
			return interfaces.ErrEmailExists
		}
		return fmt.Errorf("adopt or insert user: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return interfaces.ErrAccountVerified
	}
	return nil
}

// saveIdentityLinkOn is SaveIdentityLink's insert on exec: the pool, or SaveUserWithIdentityLink's transaction.
func saveIdentityLinkOn(ctx context.Context, exec DBTX, link *interfaces.IdentityLink) error {
	res, err := exec.ExecContext(ctx, `
		INSERT INTO identity_links (provider, subject, user_id, email, created_at)
		VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (provider, subject) DO NOTHING`,
		link.Provider, link.Subject, link.UserID, link.Email, time.Now().UTC(),
	)
	if err != nil {
		return fmt.Errorf("save identity link: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return fmt.Errorf("identity link already exists for %s subject", link.Provider)
	}
	return nil
}

// LoadIdentityLink looks up the local user mapped to an external identity.
// Returns interfaces.ErrNotFound when the identity has never been linked.
func (b *PostgresStorageBackend) LoadIdentityLink(ctx context.Context, provider, subject string) (*interfaces.IdentityLink, error) {
	row := b.query(ctx).QueryRowContext(ctx, `
		SELECT provider, subject, user_id, email, created_at
		FROM identity_links WHERE provider = $1 AND subject = $2`,
		provider, subject,
	)
	var link interfaces.IdentityLink
	if err := row.Scan(&link.Provider, &link.Subject, &link.UserID, &link.Email, &link.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, interfaces.ErrNotFound
		}
		return nil, fmt.Errorf("load identity link: %w", err)
	}
	return &link, nil
}

// ListIdentityLinks returns every IdP identity linked to a local user, for the
// "Linked identities" card in account settings. Reads use the request's
// executor (identity_links has no RLS policy) so an authenticated request does
// not take a second pooled connection; see the note atop postgres_users.go.
func (b *PostgresStorageBackend) ListIdentityLinks(ctx context.Context, userID string) ([]*interfaces.IdentityLink, error) {
	rows, err := b.query(ctx).QueryContext(ctx, `
		SELECT provider, subject, user_id, email, created_at
		FROM identity_links WHERE user_id = $1 ORDER BY created_at ASC`, userID)
	if err != nil {
		return nil, fmt.Errorf("list identity links: %w", err)
	}
	defer rows.Close()
	out := []*interfaces.IdentityLink{}
	for rows.Next() {
		var link interfaces.IdentityLink
		if err := rows.Scan(&link.Provider, &link.Subject, &link.UserID, &link.Email, &link.CreatedAt); err != nil {
			return nil, fmt.Errorf("scan identity link: %w", err)
		}
		out = append(out, &link)
	}
	return out, rows.Err()
}

// DeleteIdentityLink removes a user's link for one provider. Scoped by user_id
// so a caller can only unlink their own identities. Deleting an absent link is
// a no-op.
func (b *PostgresStorageBackend) DeleteIdentityLink(ctx context.Context, userID, provider string) error {
	if _, err := b.db.ExecContext(ctx,
		`DELETE FROM identity_links WHERE user_id = $1 AND provider = $2`, userID, provider); err != nil {
		return fmt.Errorf("delete identity link: %w", err)
	}
	return nil
}
