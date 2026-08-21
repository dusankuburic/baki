package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

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

// batchExec is the executor subset both tx paths satisfy, so the multi-row
// loop is shared between the RLS-middleware tx and the self-opened tx.
type batchExec interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// findingStatusBatchRows caps rows per multi-row upsert: 1 shared flow_id
// param + 6 per row must stay under the Postgres 65535 bind-parameter limit
// (10000 rows → 60001 params).
const findingStatusBatchRows = 9000

// buildFindingStatusUpsert renders one multi-row INSERT..ON CONFLICT chunk.
// flow_id is the shared $1; each row contributes 6 params.
func buildFindingStatusUpsert(flowID string, batch []*interfaces.FindingStatus) (string, []any) {
	var sb strings.Builder
	args := make([]any, 0, len(batch)*6+1)
	sb.WriteString(`INSERT INTO finding_status (flow_id, finding_key, rule_id, status, justification, assignee_id, updated_by, updated_at)
		VALUES `)
	args = append(args, flowID)
	for i, st := range batch {
		if i > 0 {
			sb.WriteByte(',')
		}
		base := i*6 + 2
		fmt.Fprintf(&sb, "($1,$%d,$%d,$%d,$%d,$%d,$%d,NOW())",
			base, base+1, base+2, base+3, base+4, base+5)
		args = append(args, st.FindingKey, st.RuleID, st.Status, st.Justification, st.AssigneeID, st.UpdatedBy)
	}
	sb.WriteString(`
		ON CONFLICT (flow_id, finding_key) DO UPDATE SET
			rule_id = EXCLUDED.rule_id,
			status = EXCLUDED.status,
			justification = EXCLUDED.justification,
			assignee_id = EXCLUDED.assignee_id,
			updated_by = EXCLUDED.updated_by,
			updated_at = NOW()`)
	return sb.String(), args
}

// execFindingStatusBatch runs the items as chunked multi-row upserts on exec —
// one round trip per ≤9000 rows instead of one per item.
func execFindingStatusBatch(ctx context.Context, exec batchExec, flowID string, items []*interfaces.FindingStatus) error {
	for start := 0; start < len(items); start += findingStatusBatchRows {
		end := min(start+findingStatusBatchRows, len(items))
		query, args := buildFindingStatusUpsert(flowID, items[start:end])
		if _, err := exec.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("batch triage rows %d..%d: %w", start, end, err)
		}
	}
	return nil
}

// BatchSetFindingStatus upserts multiple finding-status rows for one flow in a
// single transaction. Atomicity contract: either every row commits or none — a
// mid-batch failure rolls back the whole batch, so the audit log's "updated: N"
// and the actual persisted row count never diverge (the previous per-item loop
// silently committed items 1..K-1 on a failure at item K).
//
// Rows are written as chunked multi-row upserts (buildFindingStatusUpsert) —
// one Exec round trip per ≤9000 rows, not one per item.
//
// The tx is RLS-scoped to userID so existing per-flow RLS policies still apply.
// If the RLS middleware already opened a tx on ctx we use it directly
// (committing nested-opened transactions is the middleware's job); otherwise we
// open and commit our own.
func (b *PostgresStorageBackend) BatchSetFindingStatus(ctx context.Context, flowID, userID string, items []*interfaces.FindingStatus) error {
	if len(items) == 0 {
		return nil
	}

	// If the RLS middleware already opened a tx, run inside it (no inner
	// commit — the middleware commits or rolls back the whole request).
	if existingTx, ok := ctx.Value(rlsTxKey).(*sql.Tx); ok && existingTx != nil {
		return execFindingStatusBatch(ctx, existingTx, flowID, items)
	}

	// No existing tx — open and commit our own RLS-scoped tx.
	tx, err := b.BeginRLS(ctx, userID)
	if err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := execFindingStatusBatch(ctx, tx, flowID, items); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("batch triage commit: %w", err)
	}
	committed = true
	return nil
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
