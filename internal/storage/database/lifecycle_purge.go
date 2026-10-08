package database

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"slices"

	"pad-analyzer/internal/storage/interfaces"
)

// purgeStep removes the rows of one table whose retention has elapsed.
type purgeStep struct {
	table string
	// cond is the WHERE condition of an expired row; $1 is the retention window in days when windowed.
	cond string
	// batched: the table grows without bound, so it is deleted purgeBatchSize rows per statement (purgeBatched).
	batched bool
	// windowed: cond takes the audit retention window, and the step is skipped when there is none.
	windowed bool
	// count is the PurgeResult field the step's removals are reported in.
	count func(*interfaces.PurgeResult) *int
}

// purgeSteps returns every domain's purge steps in run order: the single statements, then the batched tables
// without a window, then the windowed ones; by table within each group.
func purgeSteps() []purgeStep {
	var out []purgeStep
	for _, d := range lifecycleDomains {
		out = append(out, d.purge...)
	}
	group := func(s purgeStep) int {
		switch {
		case !s.batched:
			return 0
		case !s.windowed:
			return 1
		default:
			return 2
		}
	}
	slices.SortStableFunc(out, func(a, b purgeStep) int {
		return cmp.Or(cmp.Compare(group(a), group(b)), cmp.Compare(a.table, b.table))
	})
	return out
}

// purgeBatchSize is how many rows one DELETE of a growth table removes
// (purgeBatched). A variable so tests can shrink it.
var purgeBatchSize = 5000

// purgeSQL renders the single-statement form of a purge step.
func purgeSQL(table, cond string) string {
	return fmt.Sprintf(`DELETE FROM %s WHERE %s`, table, cond)
}

// purgeBatchedSQL renders the batched form of `DELETE FROM table WHERE cond`: rows are picked by physical
// address (ctid) through the condition's own index, LIMIT $batch at a time, so each statement finishes well
// inside statement_timeout whatever the backlog.
func purgeBatchedSQL(table, cond, batch string) string {
	return fmt.Sprintf(`DELETE FROM %s WHERE ctid = ANY(ARRAY(SELECT ctid FROM %s WHERE %s LIMIT %s))`, table, table, cond, batch)
}

// purgeBatched deletes every row of table matching cond, purgeBatchSize rows per statement, until a statement
// removes nothing. Returns the rows removed so far with the first error, so a partial sweep is still counted.
func (b *PostgresStorageBackend) purgeBatched(ctx context.Context, table, cond string, args ...any) (int, error) {
	batchArgs := append(sqlArgs{}, args...)
	stmt := purgeBatchedSQL(table, cond, batchArgs.add(purgeBatchSize))
	total := 0
	for {
		if err := ctx.Err(); err != nil {
			return total, err
		}
		res, err := b.db.ExecContext(ctx, stmt, batchArgs...)
		if err != nil {
			return total, err
		}
		n, _ := res.RowsAffected()
		total += int(n)
		if n == 0 {
			return total, nil
		}
	}
}

// PurgeExpiredData removes stale rows whose retention has elapsed — every domain's purge steps (lifecycle_*.go), for
// a periodic background job (single-replica). auditRetentionDays <= 0 keeps the windowed tables indefinitely (only
// expired tokens, invites, links and keys purge). The tables that grow without bound are purged in batches of
// purgeBatchSize rows per statement: a single unbatched DELETE over a large backlog timed out on every tick and never
// shrank it. Each table is swept independently — an error is recorded and the remaining tables still run; the result
// carries every table's count, the error joins all failures.
func (b *PostgresStorageBackend) PurgeExpiredData(ctx context.Context, auditRetentionDays int) (*interfaces.PurgeResult, error) {
	out := &interfaces.PurgeResult{}
	var errs []error
	for _, s := range purgeSteps() {
		var args []any
		if s.windowed {
			if auditRetentionDays <= 0 {
				continue
			}
			args = []any{auditRetentionDays}
		}
		if !s.batched {
			if err := ctx.Err(); err != nil {
				errs = append(errs, fmt.Errorf("purge: %w", err))
				return out, errors.Join(errs...)
			}
			res, err := b.db.ExecContext(ctx, purgeSQL(s.table, s.cond), args...)
			if err != nil {
				errs = append(errs, fmt.Errorf("purge %s: %w", s.table, err))
				continue
			}
			n, _ := res.RowsAffected()
			*s.count(out) = int(n)
			continue
		}
		n, err := b.purgeBatched(ctx, s.table, s.cond, args...)
		*s.count(out) = n
		if err != nil {
			errs = append(errs, fmt.Errorf("purge %s: %w", s.table, err))
			if ctx.Err() != nil {
				break // nothing further can run
			}
		}
	}
	return out, errors.Join(errs...)
}
