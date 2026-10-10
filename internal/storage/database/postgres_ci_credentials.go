package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// Organisation CI credentials (migration 0038, ADR 0019). The secret column holds the whole credential string, the
// HMAC key the webhook verifies with, so it must be recoverable: it is sealed with the field encryption key
// (field_crypto.go), and without that key nothing is written. The admin routes run in the caller's request
// transaction (the org-member policy applies); the webhook's reads run on the pool before anyone is known, so every
// statement names the organisation or the key id itself.

const ciCredentialColumns = `id, org_id, name, secret, secret_hint, created_by, created_at, expires_at, last_used_at` // #nosec G101 -- column names, not a credential

// CreateCICredential inserts c unless its organisation already holds maxActive unexpired credentials. The count and
// the insert run under a per-organisation advisory lock in one transaction (the request's when there is one), so two
// concurrent creates cannot both take the last slot.
func (b *PostgresStorageBackend) CreateCICredential(ctx context.Context, c *interfaces.CICredential, maxActive int) error {
	if c == nil || c.ID == "" || c.OrgID == "" || c.Secret == "" {
		return errors.New("ci credential requires id, orgId and secret")
	}
	if b.sealer.Load() == nil {
		return interfaces.ErrSecretSealingUnavailable
	}
	secret, err := b.sealField("org_ci_credentials", "secret", c.ID, c.Secret)
	if err != nil {
		return err
	}
	if c.CreatedAt.IsZero() {
		c.CreatedAt = time.Now().UTC()
	}
	return b.withTx(ctx, func(txCtx context.Context, tx *sql.Tx) error {
		if _, err := tx.ExecContext(txCtx, `SELECT pg_advisory_xact_lock(hashtextextended('org_ci_credentials:' || $1, 0))`, c.OrgID); err != nil {
			return fmt.Errorf("ci credential: org lock: %w", err)
		}
		var active int
		if err := tx.QueryRowContext(txCtx, `SELECT count(*) FROM org_ci_credentials
			WHERE org_id = $1 AND (expires_at IS NULL OR expires_at > NOW())`, c.OrgID).Scan(&active); err != nil {
			return err
		}
		if active >= maxActive {
			return interfaces.ErrCICredentialLimit
		}
		_, err := tx.ExecContext(txCtx, `INSERT INTO org_ci_credentials
			(id, org_id, name, secret, secret_hint, created_by, created_at, expires_at)
			VALUES ($1, $2, $3, $4, $5, $6, $7, $8)`,
			c.ID, c.OrgID, c.Name, secret, c.SecretHint, c.CreatedBy, c.CreatedAt, c.ExpiresAt)
		return err
	})
}

// GetCICredential reads the credential by key id; the webhook's lookup, on the pool.
func (b *PostgresStorageBackend) GetCICredential(ctx context.Context, id string) (*interfaces.CICredential, error) {
	row := b.query(ctx).QueryRowContext(ctx, `SELECT `+ciCredentialColumns+` FROM org_ci_credentials WHERE id = $1`, id)
	c, err := b.scanCICredential(row.Scan, true)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, interfaces.ErrNotFound
	}
	return c, err
}

// ListCICredentials lists the organisation's credentials for the admin list; the secrets stay sealed and are not
// returned.
func (b *PostgresStorageBackend) ListCICredentials(ctx context.Context, orgID string) ([]*interfaces.CICredential, error) {
	return b.queryCICredentials(ctx, false, `SELECT `+ciCredentialColumns+` FROM org_ci_credentials
		WHERE org_id = $1 ORDER BY created_at DESC, id`, orgID)
}

// ActiveCICredentials reads the unexpired credentials the old-client fallback verifies against, the most recently
// used first, so a runner that signed before is matched early.
func (b *PostgresStorageBackend) ActiveCICredentials(ctx context.Context, orgID string, limit int) ([]*interfaces.CICredential, error) {
	return b.queryCICredentials(ctx, true, `SELECT `+ciCredentialColumns+` FROM org_ci_credentials
		WHERE org_id = $1 AND (expires_at IS NULL OR expires_at > NOW())
		ORDER BY last_used_at DESC NULLS LAST, created_at DESC, id LIMIT $2`, orgID, limit)
}

func (b *PostgresStorageBackend) queryCICredentials(ctx context.Context, withSecret bool, query string, args ...any) ([]*interfaces.CICredential, error) {
	rows, err := b.query(ctx).QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []*interfaces.CICredential{}
	for rows.Next() {
		c, err := b.scanCICredential(rows.Scan, withSecret)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// scanCICredential scans one row of ciCredentialColumns; withSecret opens the sealed secret, else it is dropped.
func (b *PostgresStorageBackend) scanCICredential(scan func(dest ...any) error, withSecret bool) (*interfaces.CICredential, error) {
	var c interfaces.CICredential
	if err := scan(&c.ID, &c.OrgID, &c.Name, &c.Secret, &c.SecretHint, &c.CreatedBy, &c.CreatedAt, &c.ExpiresAt, &c.LastUsedAt); err != nil {
		return nil, err
	}
	if !withSecret {
		c.Secret = ""
		return &c, nil
	}
	secret, err := b.openField("org_ci_credentials", "secret", c.ID, c.Secret)
	if err != nil {
		return nil, err
	}
	c.Secret = secret
	return &c, nil
}

// DeleteCICredential revokes a credential: a hard delete scoped to the organisation in the URL.
func (b *PostgresStorageBackend) DeleteCICredential(ctx context.Context, orgID, id string) error {
	res, err := b.query(ctx).ExecContext(ctx, `DELETE FROM org_ci_credentials WHERE id = $1 AND org_id = $2`, id, orgID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return interfaces.ErrNotFound
	}
	return nil
}

// TouchCICredential records a use, at most once per five minutes per credential, so a busy runner does not rewrite
// the row on every call.
func (b *PostgresStorageBackend) TouchCICredential(ctx context.Context, id string) error {
	_, err := b.query(ctx).ExecContext(ctx, `UPDATE org_ci_credentials SET last_used_at = NOW()
		WHERE id = $1 AND (last_used_at IS NULL OR last_used_at < NOW() - INTERVAL '5 minutes')`, id)
	return err
}

// LoadFlowHeaderInOrg is LoadFlowHeader with the organisation as an explicit predicate: the CI webhook reads on the
// pool, where row-level security cannot express an organisation's scope.
func (b *PostgresStorageBackend) LoadFlowHeaderInOrg(ctx context.Context, id, orgID string) (*interfaces.FlowDocument, error) {
	if orgID == "" {
		return nil, interfaces.ErrNotFound
	}
	var flow interfaces.FlowDocument
	var metaRaw []byte
	var tagsCSV string
	if err := b.query(ctx).QueryRowContext(ctx, `SELECT id, name, description, metadata, owner_id, org_id, created_at, updated_at, version, tags
		FROM flows WHERE id = $1 AND org_id = $2`, id, orgID).Scan(
		&flow.ID, &flow.Name, &flow.Description, &metaRaw,
		&flow.OwnerID, &flow.OrganizationID,
		&flow.CreatedAt, &flow.UpdatedAt, &flow.Version,
		&tagsCSV,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, interfaces.ErrNotFound
		}
		return nil, fmt.Errorf("scan flow header: %w", err)
	}
	if err := json.Unmarshal(metaRaw, &flow.Metadata); err != nil {
		return nil, fmt.Errorf("unmarshal metadata: %w", err)
	}
	flow.Tags = interfaces.SplitFlowTags(tagsCSV)
	return &flow, nil
}

// RecordCIDelivery inserts the webhook's replay record on the pool (migration 0039: the table is visible outside any
// user's request transaction only). ON CONFLICT DO NOTHING decides a race on the primary key: of two concurrent
// inserts of one pair, the second waits for the first and inserts nothing. The same statement removes the caller's
// records past ciDeliveryRetention, so a key's record stays bounded where no retention sweep runs (local+db without
// accounts). Whether an id that old counts as recorded once more is left to the statement's order; either answer
// is safe, as the signature that carried it expired long before.
func (b *PostgresStorageBackend) RecordCIDelivery(ctx context.Context, caller, deliveryID string) (bool, error) {
	res, err := b.query(ctx).ExecContext(ctx, `WITH expired AS (
			DELETE FROM ci_webhook_deliveries WHERE caller = $1 AND received_at < NOW() - `+ciDeliveryRetention+`
		)
		INSERT INTO ci_webhook_deliveries (caller, delivery_id) VALUES ($1, $2)
		ON CONFLICT (caller, delivery_id) DO NOTHING`, caller, deliveryID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return n == 1, err
}
