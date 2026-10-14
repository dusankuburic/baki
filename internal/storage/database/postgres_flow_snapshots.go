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

// The cloud undo ring (migration 0045). Every statement goes through b.query(ctx), so a request's row-level-security
// scope holds: the policy admits the snapshots of the flows the caller may open.

// PushFlowSnapshot inserts s and deletes the flow's older snapshots beyond lim in the same unit of work. Inside a
// request's transaction the unit is a SAVEPOINT: the snapshot is an undo affordance, and a failed insert (a vanished
// flow, a policy refusal) must not abort the transaction of the mutation it follows.
func (b *PostgresStorageBackend) PushFlowSnapshot(ctx context.Context, s *interfaces.FlowSnapshot, lim interfaces.FlowSnapshotLimits) error {
	if err := validFlowID(s.FlowID); err != nil {
		return err
	}
	if flowSnapshotHasNUL(s) {
		return interfaces.ErrFlowSnapshotUnstorable
	}
	retained := s.StoredBytes()
	if lim.MaxBytes > 0 && retained > lim.MaxBytes {
		return interfaces.ErrFlowSnapshotTooLarge
	}
	var files any // NULL for a single-file snapshot
	if s.Files != nil {
		var err error
		if files, err = json.Marshal(s.Files); err != nil {
			return fmt.Errorf("marshal snapshot files: %w", err)
		}
	}
	keep, maxBytes := lim.Keep, lim.MaxBytes
	if keep <= 0 {
		keep = 1 << 30 // no count bound asked for
	}
	if maxBytes <= 0 {
		maxBytes = 1 << 62
	}
	err := b.inSavepoint(ctx, "flow_snapshot_push", func(q DBTX) error {
		// created_at keeps the caller's stamp when it gave one (the zero time is the column's NOW()).
		var created any
		if !s.CreatedAt.IsZero() {
			created = s.CreatedAt.UTC()
		}
		if _, err := q.ExecContext(ctx,
			`INSERT INTO flow_snapshots (id, flow_id, label, bytes, retained_bytes, source, files, created_at)
			 VALUES ($1, $2, $3, $4, $5, $6, $7, COALESCE($8::timestamptz, NOW()))`,
			s.ID, s.FlowID, s.Label, s.Bytes, retained, s.Source, files, created); err != nil {
			return err
		}
		// Newest first, a row is kept while it is within the count and the running size is within the bound; the
		// newest always is (a snapshot over the bound alone was refused above).
		_, err := q.ExecContext(ctx, `
			DELETE FROM flow_snapshots WHERE flow_id = $1 AND seq IN (
				SELECT seq FROM (
					SELECT seq,
					       ROW_NUMBER() OVER (ORDER BY seq DESC) AS n,
					       SUM(retained_bytes) OVER (ORDER BY seq DESC) AS running
					  FROM flow_snapshots WHERE flow_id = $1
				) ring WHERE n > $2::bigint OR running > $3::bigint)`,
			s.FlowID, keep, maxBytes)
		return err
	})
	// A flow that is gone (foreign key) or that the caller's scope cannot see (row-level security) has no ring.
	if isPgErrCode(err, pgErrForeignKeyViolation) || isPgErrCode(err, pgErrInsufficientPrivilege) {
		return interfaces.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("push flow snapshot: %w", err)
	}
	return nil
}

// flowSnapshotHasNUL reports whether any text of s holds a NUL byte, which TEXT and JSONB both refuse.
func flowSnapshotHasNUL(s *interfaces.FlowSnapshot) bool {
	if strings.IndexByte(s.Source, 0) >= 0 {
		return true
	}
	for name, text := range s.Files {
		if strings.IndexByte(name, 0) >= 0 || strings.IndexByte(text, 0) >= 0 {
			return true
		}
	}
	return false
}

// flowSnapshotLive is the condition of a snapshot still offered; $2 is the TTL in seconds.
const flowSnapshotLive = `created_at > NOW() - ($2::bigint * INTERVAL '1 second')`

// ListFlowSnapshots returns the flow's live snapshots oldest first, without source and files.
func (b *PostgresStorageBackend) ListFlowSnapshots(ctx context.Context, flowID string) ([]*interfaces.FlowSnapshot, error) {
	rows, err := b.query(ctx).QueryContext(ctx,
		`SELECT id, label, bytes, created_at FROM flow_snapshots WHERE flow_id = $1 AND `+flowSnapshotLive+` ORDER BY seq`,
		flowID, int64(interfaces.FlowSnapshotTTL.Seconds()))
	if err != nil {
		return nil, fmt.Errorf("list flow snapshots: %w", err)
	}
	defer rows.Close()
	out := []*interfaces.FlowSnapshot{}
	for rows.Next() {
		s := &interfaces.FlowSnapshot{FlowID: flowID}
		if err := rows.Scan(&s.ID, &s.Label, &s.Bytes, &s.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// LoadFlowSnapshot returns the live snapshot id of flowID with its text; ErrNotFound for any other combination.
func (b *PostgresStorageBackend) LoadFlowSnapshot(ctx context.Context, flowID, id string) (*interfaces.FlowSnapshot, error) {
	s := &interfaces.FlowSnapshot{FlowID: flowID}
	var files []byte
	err := b.query(ctx).QueryRowContext(ctx,
		`SELECT id, label, bytes, source, files, created_at FROM flow_snapshots
		  WHERE flow_id = $1 AND id = $3 AND `+flowSnapshotLive,
		flowID, int64(interfaces.FlowSnapshotTTL.Seconds()), id,
	).Scan(&s.ID, &s.Label, &s.Bytes, &s.Source, &files, &s.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, interfaces.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("load flow snapshot: %w", err)
	}
	if files != nil {
		if err := json.Unmarshal(files, &s.Files); err != nil {
			return nil, fmt.Errorf("decode snapshot files of %s: %w", id, err)
		}
	}
	return s, nil
}

// inSavepoint runs fn in the request's transaction under a SAVEPOINT, rolled back to when fn fails so the
// transaction stays usable, or on the pool (one statement each) when there is no request scope.
func (b *PostgresStorageBackend) inSavepoint(ctx context.Context, name string, fn func(q DBTX) error) error {
	tx, ok, err := rlsTxFromContext(ctx)
	if err != nil {
		return err
	}
	if !ok {
		return b.withTx(ctx, func(_ context.Context, tx *sql.Tx) error { return fn(tx) })
	}
	if _, err := tx.ExecContext(ctx, `SAVEPOINT `+name); err != nil {
		return fmt.Errorf("savepoint %s: %w", name, err)
	}
	if err := fn(tx); err != nil {
		if _, rbErr := tx.ExecContext(ctx, `ROLLBACK TO SAVEPOINT `+name); rbErr != nil {
			return errors.Join(err, fmt.Errorf("rollback to savepoint %s: %w", name, rbErr))
		}
		_, _ = tx.ExecContext(ctx, `RELEASE SAVEPOINT `+name)
		return err
	}
	if _, err := tx.ExecContext(ctx, `RELEASE SAVEPOINT `+name); err != nil {
		return fmt.Errorf("release savepoint %s: %w", name, err)
	}
	return nil
}

// pgErrInsufficientPrivilege is the SQLSTATE of a row refused by a WITH CHECK policy.
const pgErrInsufficientPrivilege = "42501"
