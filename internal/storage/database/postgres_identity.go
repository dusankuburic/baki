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
	row := b.db.QueryRowContext(ctx, `
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
