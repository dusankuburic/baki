package database

import (
	"context"
	"database/sql"
	"encoding/json"

	"pad-core/models"

	"pad-analyzer/internal/storage/interfaces"
)

// SavePolicy inserts or updates a policy (upsert by id+org_id).
func (b *PostgresStorageBackend) SavePolicy(ctx context.Context, p *models.Policy) error {
	rulesJSON, err := json.Marshal(p.Rules)
	if err != nil {
		return err
	}
	_, err = b.query(ctx).ExecContext(ctx,
		`INSERT INTO policies (id, org_id, name, description, rules, gate_severity, created_at, updated_at)
		 VALUES ($1, $2, $3, $4, $5, $6, NOW(), NOW())
		 ON CONFLICT (id, org_id) DO UPDATE SET
		   name = EXCLUDED.name,
		   description = EXCLUDED.description,
		   rules = EXCLUDED.rules,
		   gate_severity = EXCLUDED.gate_severity,
		   updated_at = NOW()`,
		p.ID, p.OrgID, p.Name, p.Description, rulesJSON, string(p.GateSeverity))
	return err
}

// GetPolicy retrieves a single policy by id within an org.
func (b *PostgresStorageBackend) GetPolicy(ctx context.Context, orgID, id string) (*models.Policy, error) {
	var p models.Policy
	var rulesRaw []byte
	var gate, orgIDOut string
	err := b.query(ctx).QueryRowContext(ctx,
		`SELECT id, org_id, name, description, rules, gate_severity, created_at, updated_at
		 FROM policies WHERE id = $1 AND org_id = $2`, id, orgID).
		Scan(&p.ID, &orgIDOut, &p.Name, &p.Description, &rulesRaw, &gate, &p.CreatedAt, &p.UpdatedAt)
	if err == sql.ErrNoRows {
		return nil, interfaces.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	p.OrgID = orgIDOut
	p.GateSeverity = models.Severity(gate)
	if err := json.Unmarshal(rulesRaw, &p.Rules); err != nil {
		return nil, err
	}
	return &p, nil
}

// ListPolicies returns all policies for an org, ordered by name.
func (b *PostgresStorageBackend) ListPolicies(ctx context.Context, orgID string) ([]*models.Policy, error) {
	rows, err := b.query(ctx).QueryContext(ctx,
		`SELECT id, org_id, name, description, rules, gate_severity, created_at, updated_at
		 FROM policies WHERE org_id = $1 ORDER BY name`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var policies []*models.Policy
	for rows.Next() {
		var p models.Policy
		var rulesRaw []byte
		var gate, orgIDOut string
		if err := rows.Scan(&p.ID, &orgIDOut, &p.Name, &p.Description, &rulesRaw, &gate, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		p.OrgID = orgIDOut
		p.GateSeverity = models.Severity(gate)
		if err := json.Unmarshal(rulesRaw, &p.Rules); err != nil {
			return nil, err
		}
		policies = append(policies, &p)
	}
	return policies, rows.Err()
}

// DeletePolicy removes a policy, scoped to orgID.
func (b *PostgresStorageBackend) DeletePolicy(ctx context.Context, orgID, id string) error {
	_, err := b.query(ctx).ExecContext(ctx,
		`DELETE FROM policies WHERE id = $1 AND org_id = $2`, id, orgID)
	return err
}
