package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"pad-core/models"

	"pad-analyzer/internal/storage/interfaces"
)

// policyColumns are a policy row's columns in the order scanPolicy reads them.
const policyColumns = `id, org_id, name, description, rules, gate_severity, waivers, max_errors, max_warnings, max_info, created_at, updated_at`

// SavePolicy inserts or updates a policy (upsert by id+org_id).
func (b *PostgresStorageBackend) SavePolicy(ctx context.Context, p *models.Policy) error {
	rulesJSON, err := json.Marshal(p.Rules)
	if err != nil {
		return err
	}
	// No waivers is NULL (v36), like the rows written before the column existed.
	var waiversJSON any
	if len(p.Waivers) > 0 {
		raw, err := json.Marshal(p.Waivers)
		if err != nil {
			return err
		}
		waiversJSON = raw
	}
	// RETURNING the stamps, and writing them back onto p: the row's timestamps
	// are set here (NOW()), and the save handler renders the struct it was
	// given — so without this a just-saved policy came back with a zero
	// created/updated date and the panel showed the year 1 until a refetch.
	return b.query(ctx).QueryRowContext(ctx,
		`INSERT INTO policies (id, org_id, name, description, rules, gate_severity, waivers, max_errors, max_warnings, max_info, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, NOW(), NOW())
		 ON CONFLICT (id, org_id) DO UPDATE SET
		   name = EXCLUDED.name,
		   description = EXCLUDED.description,
		   rules = EXCLUDED.rules,
		   gate_severity = EXCLUDED.gate_severity,
		   waivers = EXCLUDED.waivers,
		   max_errors = EXCLUDED.max_errors,
		   max_warnings = EXCLUDED.max_warnings,
		   max_info = EXCLUDED.max_info,
		   updated_at = NOW()
		 RETURNING created_at, updated_at`,
		p.ID, p.OrgID, p.Name, p.Description, rulesJSON, string(p.GateSeverity), waiversJSON, p.MaxErrors, p.MaxWarnings, p.MaxInfo,
	).Scan(&p.CreatedAt, &p.UpdatedAt)
}

// scanPolicy reads one row of policyColumns.
func scanPolicy(row interface{ Scan(dest ...any) error }) (*models.Policy, error) {
	var p models.Policy
	var rulesRaw, waiversRaw []byte
	var gate string
	if err := row.Scan(&p.ID, &p.OrgID, &p.Name, &p.Description, &rulesRaw, &gate, &waiversRaw,
		&p.MaxErrors, &p.MaxWarnings, &p.MaxInfo, &p.CreatedAt, &p.UpdatedAt); err != nil {
		return nil, err
	}
	p.GateSeverity = models.Severity(gate)
	if err := json.Unmarshal(rulesRaw, &p.Rules); err != nil {
		return nil, err
	}
	if waiversRaw != nil {
		if err := json.Unmarshal(waiversRaw, &p.Waivers); err != nil {
			return nil, err
		}
	}
	return &p, nil
}

// GetPolicy retrieves a single policy by id within an org.
func (b *PostgresStorageBackend) GetPolicy(ctx context.Context, orgID, id string) (*models.Policy, error) {
	p, err := scanPolicy(b.query(ctx).QueryRowContext(ctx,
		`SELECT `+policyColumns+` FROM policies WHERE id = $1 AND org_id = $2`, id, orgID))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, interfaces.ErrNotFound
	}
	return p, err
}

// ListPolicies returns all policies for an org, ordered by name.
func (b *PostgresStorageBackend) ListPolicies(ctx context.Context, orgID string) ([]*models.Policy, error) {
	rows, err := b.query(ctx).QueryContext(ctx,
		`SELECT `+policyColumns+` FROM policies WHERE org_id = $1 ORDER BY name`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var policies []*models.Policy
	for rows.Next() {
		p, err := scanPolicy(rows)
		if err != nil {
			return nil, err
		}
		policies = append(policies, p)
	}
	return policies, rows.Err()
}

// DeletePolicy removes a policy, scoped to orgID.
func (b *PostgresStorageBackend) DeletePolicy(ctx context.Context, orgID, id string) error {
	_, err := b.query(ctx).ExecContext(ctx,
		`DELETE FROM policies WHERE id = $1 AND org_id = $2`, id, orgID)
	return err
}
