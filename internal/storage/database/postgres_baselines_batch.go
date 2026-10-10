package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"pad-analyzer/internal/storage/interfaces"
)

// GetFlowBaselinesBatch resolves the baselines of many flows in one query.
// The governance scanner reads a page of flows at a time; one GetFlowBaseline
// per flow was two round trips per flow per sweep (with LoadFlowHealth).
// Flows without a baseline are absent from the map, which is never nil. Row
// decoding matches GetFlowBaseline (postgres_triage.go): Keys is never nil.
// A row carries its rule set's digest, not the rule set (RuleSetsByDigest).
func (b *PostgresStorageBackend) GetFlowBaselinesBatch(ctx context.Context, flowIDs []string) (map[string]*interfaces.FlowBaseline, error) {
	out := make(map[string]*interfaces.FlowBaseline, len(flowIDs))
	if len(flowIDs) == 0 {
		return out, nil
	}
	rows, err := b.query(ctx).QueryContext(ctx,
		`SELECT flow_id, keys, created_by, created_at, CASE WHEN rule_set_at = created_at THEN rule_set_digest END
		   FROM flow_baselines WHERE flow_id = ANY($1)`, flowIDs)
	if err != nil {
		return nil, fmt.Errorf("load flow baselines batch: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var bl interfaces.FlowBaseline
		var keysJSON []byte
		var digest sql.NullString
		if err := rows.Scan(&bl.FlowID, &keysJSON, &bl.CreatedBy, &bl.CreatedAt, &digest); err != nil {
			return nil, fmt.Errorf("scan flow baselines batch: %w", err)
		}
		bl.RuleSetDigest = digest.String
		if len(keysJSON) > 0 {
			if err := json.Unmarshal(keysJSON, &bl.Keys); err != nil {
				return nil, fmt.Errorf("decode flow baseline %s keys: %w", bl.FlowID, err)
			}
		}
		if bl.Keys == nil {
			bl.Keys = []string{}
		}
		row := bl
		out[bl.FlowID] = &row
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("load flow baselines batch: %w", err)
	}
	return out, nil
}
