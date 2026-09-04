package database

import (
	"context"
	"errors"
	"fmt"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// Per-org custom analyzer rules (R4). See migration v21 for the table + RLS
// policy. Every method is scoped by org_id in SQL as well as by RLS: RLS is
// inert whenever the app connects as a superuser/BYPASSRLS role, so the
// explicit predicate has to hold on its own (the lesson from B8).

// SaveOrgCustomRule upserts a rule keyed on (org_id, rule_id).
//
// The conflict target is the COMPOSITE (org_id, rule_id), not the surrogate id.
// That is what makes "org A and org B both define house-style" two distinct
// rules rather than one racing pair — and it means a re-save under the same org
// replaces in place, keeping created_at.
//
// It deliberately does NOT conflict on id: an id belonging to another org must
// not be updatable from this org's endpoint. B8.2 was exactly that bug on
// org_channels (ON CONFLICT (id) with no org predicate let an admin retarget
// another tenant's row), so the guard is carried forward here by construction.
func (b *PostgresStorageBackend) SaveOrgCustomRule(ctx context.Context, rule *storageif.OrgCustomRule) error {
	if rule == nil || rule.ID == "" || rule.OrgID == "" || rule.RuleID == "" {
		return errors.New("org custom rule requires id, orgId and ruleId")
	}
	var savedID string
	err := b.query(ctx).QueryRowContext(ctx, `
		INSERT INTO org_custom_rules (id, org_id, rule_id, config, enabled, created_at, updated_at)
		VALUES ($1, $2, $3, $4::jsonb, $5, NOW(), NOW())
		ON CONFLICT (org_id, rule_id) DO UPDATE SET
			config     = EXCLUDED.config,
			enabled    = EXCLUDED.enabled,
			updated_at = NOW()
		RETURNING id`,
		rule.ID, rule.OrgID, rule.RuleID, string(rule.Config), rule.Enabled,
	).Scan(&savedID)
	if err != nil {
		return fmt.Errorf("save org custom rule: %w", err)
	}
	// The upsert may have matched an existing row, whose surrogate id wins.
	// Report it back so the caller persists the real identity rather than the
	// one it proposed.
	rule.ID = savedID
	return nil
}

// DeleteOrgCustomRule removes one rule. Scoped by org so an id from another
// tenant is a no-op rather than a cross-tenant delete.
func (b *PostgresStorageBackend) DeleteOrgCustomRule(ctx context.Context, orgID, id string) error {
	_, err := b.query(ctx).ExecContext(ctx,
		`DELETE FROM org_custom_rules WHERE id = $1 AND org_id = $2`, id, orgID)
	if err != nil {
		return fmt.Errorf("delete org custom rule: %w", err)
	}
	return nil
}

// ListOrgCustomRules returns the org's rules, newest-authored last so the
// analysis path compiles them in a stable order.
//
// The ordering ends in `id` as a unique tiebreaker: created_at ties whenever
// several rules are authored in the same microsecond (an import, a seeded
// org), and B9/B11 established that a non-total ordering makes any paginated
// or repeated read non-deterministic.
func (b *PostgresStorageBackend) ListOrgCustomRules(ctx context.Context, orgID string, enabledOnly bool) ([]*storageif.OrgCustomRule, error) {
	q := `SELECT id, org_id, rule_id, config, enabled, created_at, updated_at
	      FROM org_custom_rules WHERE org_id = $1`
	if enabledOnly {
		q += ` AND enabled`
	}
	q += ` ORDER BY created_at ASC, id ASC`

	rows, err := b.query(ctx).QueryContext(ctx, q, orgID)
	if err != nil {
		return nil, fmt.Errorf("list org custom rules: %w", err)
	}
	defer rows.Close()

	out := make([]*storageif.OrgCustomRule, 0)
	for rows.Next() {
		r := &storageif.OrgCustomRule{}
		var cfg []byte
		if err := rows.Scan(&r.ID, &r.OrgID, &r.RuleID, &cfg, &r.Enabled, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan org custom rule: %w", err)
		}
		r.Config = cfg
		out = append(out, r)
	}
	return out, rows.Err()
}
