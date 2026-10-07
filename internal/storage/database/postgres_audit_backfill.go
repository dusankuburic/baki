package database

import (
	"context"
	"fmt"
)

// Batch sizes of the audit org_id backfill: audit rows per statement (both phases), flows per keyset page.
const (
	auditOrgBackfillRows  = 1000
	auditOrgBackfillFlows = 500
)

// auditOrgRowsSQL fills one batch of org resources' rows: the organisation is the resource itself.
const auditOrgRowsSQL = `WITH batch AS (
	SELECT id FROM audit_events
	 WHERE resource_type = 'org' AND org_id IS NULL AND resource_id <> ''
	 LIMIT $1
)
UPDATE audit_events a SET org_id = a.resource_id
  FROM batch b
 WHERE a.id = b.id AND a.org_id IS NULL`

// auditFlowPageSQL is one keyset page of organisation flows (ids after $1). Walking the flows instead of the
// audit log keeps a rerun from re-reading the rows that never get an organisation (personal and deleted flows).
const auditFlowPageSQL = `SELECT id FROM flows WHERE org_id <> '' AND id > $1 ORDER BY id LIMIT $2`

// auditFlowRowsSQL fills at most $2 rows of the page's flows ($1) with the flow's organisation, probing
// audit_events_resource_created_idx per flow. A statement is bounded by audit rows, not by flows: a page of busy
// flows can hold more rows than one statement rewrites within the pool's statement_timeout, and a page that
// always timed out would stop the backfill at the same flow on every boot.
const auditFlowRowsSQL = `WITH batch AS (
	SELECT a.id, f.org_id FROM audit_events a
	  JOIN flows f ON f.id = a.resource_id
	 WHERE a.resource_type = 'flow' AND a.resource_id = ANY($1) AND a.org_id IS NULL AND f.org_id <> ''
	 LIMIT $2
)
UPDATE audit_events a SET org_id = b.org_id
  FROM batch b
 WHERE a.id = b.id AND a.org_id IS NULL`

// BackfillAuditOrgIDs fills audit_events.org_id on the rows written without one (before v33, and before the
// audit call sites passed the organisation): an org resource's row gets its
// resource_id, a flow's row the flow's current organisation; every other row keeps NULL. Each batch commits
// on its own, and only rows still without an organisation are written, so the backfill is idempotent and an
// interrupted run loses nothing: the next one continues with what is left. It runs on the pool, outside any
// request's RLS scope, as the maintenance job it is. Returns the rows filled.
func (b *PostgresStorageBackend) BackfillAuditOrgIDs(ctx context.Context) (int, error) {
	return b.backfillAuditOrgIDs(ctx, auditOrgBackfillRows, auditOrgBackfillFlows, nil)
}

// backfillAuditOrgIDs is BackfillAuditOrgIDs with its batch sizes and a hook called after every committed
// batch (tests interrupt the run there).
func (b *PostgresStorageBackend) backfillAuditOrgIDs(ctx context.Context, rows, flows int, afterBatch func()) (int, error) {
	filled := 0
	for {
		if err := ctx.Err(); err != nil {
			return filled, err
		}
		res, err := b.db.ExecContext(ctx, auditOrgRowsSQL, rows)
		if err != nil {
			return filled, fmt.Errorf("backfill audit org ids (org rows): %w", err)
		}
		n, _ := res.RowsAffected()
		filled += int(n)
		if afterBatch != nil {
			afterBatch()
		}
		if n < int64(rows) {
			break
		}
	}
	after := ""
	for {
		if err := ctx.Err(); err != nil {
			return filled, err
		}
		page, err := b.auditFlowPage(ctx, after, flows)
		if err != nil {
			return filled, err
		}
		for len(page) > 0 {
			if err := ctx.Err(); err != nil {
				return filled, err
			}
			res, err := b.db.ExecContext(ctx, auditFlowRowsSQL, page, rows)
			if err != nil {
				return filled, fmt.Errorf("backfill audit org ids (flow rows): %w", err)
			}
			n, _ := res.RowsAffected()
			filled += int(n)
			if afterBatch != nil {
				afterBatch()
			}
			if n < int64(rows) {
				break
			}
		}
		if len(page) < flows {
			return filled, nil
		}
		after = page[len(page)-1]
	}
}

// auditFlowPage returns the ids of the organisation flows after `after`, at most limit of them, in id order.
func (b *PostgresStorageBackend) auditFlowPage(ctx context.Context, after string, limit int) ([]string, error) {
	rs, err := b.db.QueryContext(ctx, auditFlowPageSQL, after, limit)
	if err != nil {
		return nil, fmt.Errorf("backfill audit org ids (flow page): %w", err)
	}
	defer rs.Close()
	var ids []string
	for rs.Next() {
		var id string
		if err := rs.Scan(&id); err != nil {
			return nil, fmt.Errorf("backfill audit org ids (flow page): %w", err)
		}
		ids = append(ids, id)
	}
	if err := rs.Err(); err != nil {
		return nil, fmt.Errorf("backfill audit org ids (flow page): %w", err)
	}
	return ids, nil
}
