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
	res, err := b.db.ExecContext(ctx, `
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
