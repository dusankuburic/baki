package database

import (
	"context"
	"errors"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// Per-org notification channels (R2-3). RLS mirrors the knowledge tables
// (org-member visibility; see migration v17) — the HTTP layer additionally
// requires org admin for mutations.

func (b *PostgresStorageBackend) SaveOrgChannel(ctx context.Context, ch *storageif.OrgChannel) error {
	if ch == nil || ch.ID == "" || ch.OrgID == "" {
		return errors.New("org channel requires id and orgId")
	}
	_, err := b.query(ctx).ExecContext(ctx, `
		INSERT INTO org_channels (id, org_id, name, kind, url, secret, enabled, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (id) DO UPDATE SET
			name    = EXCLUDED.name,
			kind    = EXCLUDED.kind,
			url     = EXCLUDED.url,
			secret  = EXCLUDED.secret,
			enabled = EXCLUDED.enabled`,
		ch.ID, ch.OrgID, ch.Name, ch.Kind, ch.URL, ch.Secret, ch.Enabled, ch.CreatedAt)
	return err
}

func (b *PostgresStorageBackend) DeleteOrgChannel(ctx context.Context, orgID, id string) error {
	_, err := b.query(ctx).ExecContext(ctx,
		`DELETE FROM org_channels WHERE id = $1 AND org_id = $2`, id, orgID)
	if err == nil {
		return nil
	}
	return err
}

func (b *PostgresStorageBackend) ListOrgChannels(ctx context.Context, orgID string, enabledOnly bool) ([]*storageif.OrgChannel, error) {
	q := `SELECT id, org_id, name, kind, url, secret, enabled, created_at
	      FROM org_channels WHERE org_id = $1`
	if enabledOnly {
		q += ` AND enabled`
	}
	q += ` ORDER BY created_at`
	rows, err := b.query(ctx).QueryContext(ctx, q, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := []*storageif.OrgChannel{}
	for rows.Next() {
		var ch storageif.OrgChannel
		if err := rows.Scan(&ch.ID, &ch.OrgID, &ch.Name, &ch.Kind, &ch.URL, &ch.Secret, &ch.Enabled, &ch.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &ch)
	}
	return out, rows.Err()
}
