package database

import (
	"context"
	"database/sql"
	"errors"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// Per-org notification channels (R2-3). RLS mirrors the knowledge tables
// (org-member visibility; see migration v17) — the HTTP layer additionally
// requires org admin for mutations.

// SaveOrgChannel upserts one channel, keyed on its id.
//
// The conflict branch is scoped to the row's OWNING org. Without that predicate
// the id alone decided the update, and the handler takes the id from the
// request body while authorizing against the org in the URL — so an admin of
// org A could POST org B's channel id to /api/orgs/A/channels and replace B's
// webhook url + secret with their own. org_id is not in the SET list, so the
// row kept B's ownership while quietly delivering B's governance alerts to the
// attacker. RLS rejects that too, but RLS is off whenever the app connects as a
// superuser/BYPASSRLS role, so this clause has to hold on its own.
//
// A conflicting id under a different org returns ErrNotFound rather than
// succeeding silently: the WHERE alone would make both the attack AND a
// legitimate failed save look like success. ErrNotFound (not a permission
// error) so the response doesn't confirm that the id exists.
func (b *PostgresStorageBackend) SaveOrgChannel(ctx context.Context, ch *storageif.OrgChannel) error {
	if ch == nil || ch.ID == "" || ch.OrgID == "" {
		return errors.New("org channel requires id and orgId")
	}
	var savedID string
	err := b.query(ctx).QueryRowContext(ctx, `
		INSERT INTO org_channels (id, org_id, name, kind, url, secret, enabled, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		ON CONFLICT (id) DO UPDATE SET
			name    = EXCLUDED.name,
			kind    = EXCLUDED.kind,
			url     = EXCLUDED.url,
			secret  = EXCLUDED.secret,
			enabled = EXCLUDED.enabled
		WHERE org_channels.org_id = EXCLUDED.org_id
		RETURNING id`,
		ch.ID, ch.OrgID, ch.Name, ch.Kind, ch.URL, ch.Secret, ch.Enabled, ch.CreatedAt,
	).Scan(&savedID)
	if errors.Is(err, sql.ErrNoRows) {
		return storageif.ErrNotFound
	}
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
