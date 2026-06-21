package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"pad-analyzer/internal/storage/interfaces"
)

// Finding triage & baselines (cloud / multi-user Postgres mode). Rows are
// visible per the flow's RLS policy (owner / collaborator / org member); see
// finding_status and flow_baselines in postgres_migrations.go.

func (b *PostgresStorageBackend) SetFindingStatus(ctx context.Context, st *interfaces.FindingStatus) error {
	_, err := b.query(ctx).ExecContext(ctx, `
		INSERT INTO finding_status (flow_id, finding_key, rule_id, status, justification, assignee_id, updated_by, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, NOW())
		ON CONFLICT (flow_id, finding_key) DO UPDATE SET
			rule_id = EXCLUDED.rule_id,
			status = EXCLUDED.status,
			justification = EXCLUDED.justification,
			assignee_id = EXCLUDED.assignee_id,
			updated_by = EXCLUDED.updated_by,
			updated_at = NOW()`,
		st.FlowID, st.FindingKey, st.RuleID, st.Status, st.Justification, st.AssigneeID, st.UpdatedBy)
	return err
}

func (b *PostgresStorageBackend) ListFindingStatuses(ctx context.Context, flowID string) ([]*interfaces.FindingStatus, error) {
	rows, err := b.query(ctx).QueryContext(ctx, `
		SELECT flow_id, finding_key, rule_id, status, justification, assignee_id, updated_by, updated_at
		FROM finding_status WHERE flow_id = $1 ORDER BY finding_key`, flowID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]*interfaces.FindingStatus, 0)
	for rows.Next() {
		var s interfaces.FindingStatus
		if err := rows.Scan(&s.FlowID, &s.FindingKey, &s.RuleID, &s.Status, &s.Justification, &s.AssigneeID, &s.UpdatedBy, &s.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, &s)
	}
	return out, rows.Err()
}

func (b *PostgresStorageBackend) DeleteFindingStatus(ctx context.Context, flowID, findingKey string) error {
	_, err := b.query(ctx).ExecContext(ctx,
		`DELETE FROM finding_status WHERE flow_id = $1 AND finding_key = $2`, flowID, findingKey)
	return err
}

func (b *PostgresStorageBackend) GetFlowBaseline(ctx context.Context, flowID string) (*interfaces.FlowBaseline, error) {
	var bl interfaces.FlowBaseline
	var keysJSON []byte
	err := b.query(ctx).QueryRowContext(ctx,
		`SELECT flow_id, keys, created_by, created_at FROM flow_baselines WHERE flow_id = $1`, flowID).
		Scan(&bl.FlowID, &keysJSON, &bl.CreatedBy, &bl.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if len(keysJSON) > 0 {
		if err := json.Unmarshal(keysJSON, &bl.Keys); err != nil {
			return nil, err
		}
	}
	if bl.Keys == nil {
		bl.Keys = []string{}
	}
	return &bl, nil
}

func (b *PostgresStorageBackend) SetFlowBaseline(ctx context.Context, bl *interfaces.FlowBaseline) error {
	keys := bl.Keys
	if keys == nil {
		keys = []string{}
	}
	keysJSON, err := json.Marshal(keys)
	if err != nil {
		return err
	}
	_, err = b.query(ctx).ExecContext(ctx, `
		INSERT INTO flow_baselines (flow_id, keys, created_by, created_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (flow_id) DO UPDATE SET
			keys = EXCLUDED.keys,
			created_by = EXCLUDED.created_by,
			created_at = NOW()`,
		bl.FlowID, keysJSON, bl.CreatedBy)
	return err
}

func (b *PostgresStorageBackend) ClearFlowBaseline(ctx context.Context, flowID string) error {
	_, err := b.query(ctx).ExecContext(ctx, `DELETE FROM flow_baselines WHERE flow_id = $1`, flowID)
	return err
}
