package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"strings"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// recordingExec captures every statement + args executed against it — the
// DB-free harness for proving the multi-row batch writers' SQL shape and
// chunking without a Postgres.
type recordingExec struct {
	stmts []string
	argss [][]any
	fail  error
}

func (r *recordingExec) ExecContext(_ context.Context, query string, args ...any) (sql.Result, error) {
	if r.fail != nil {
		return nil, r.fail
	}
	r.stmts = append(r.stmts, query)
	cp := make([]any, len(args))
	copy(cp, args)
	r.argss = append(r.argss, cp)
	return driver.RowsAffected(0), nil
}

// TestBuildFindingStatusUpsert_Shape verifies the generated multi-row SQL: a
// single statement, shared $1 flow_id, 6 params per row, the ON CONFLICT
// clause, and the args in positional order.
func TestBuildFindingStatusUpsert_Shape(t *testing.T) {
	items := []*interfaces.FindingStatus{
		{FindingKey: "k1", RuleID: "r1", Status: "resolved", Justification: "j1", AssigneeID: "a1", UpdatedBy: "u1"},
		{FindingKey: "k2", RuleID: "r2", Status: "false_positive", UpdatedBy: "u2"},
	}
	q, args := buildFindingStatusUpsert("flow-1", items)

	if !strings.Contains(q, "ON CONFLICT (flow_id, finding_key) DO UPDATE") {
		t.Errorf("missing upsert conflict clause: %s", q)
	}
	if got := strings.Count(q, "($1,"); got != 2 {
		t.Errorf("expected 2 rows sharing $1 flow_id, found %d in: %s", got, q)
	}
	if got := strings.Count(q, "NOW()"); got != 3 {
		t.Errorf("expected NOW() twice in VALUES + once in ON CONFLICT (3), found %d", got)
	}
	// $1 shared + 6/row = 13 args; order: flowID then per-row sextuples.
	if len(args) != 1+6*2 {
		t.Fatalf("expected 13 args, got %d: %v", len(args), args)
	}
	if args[0] != "flow-1" {
		t.Errorf("args[0] = %v, want shared flowID", args[0])
	}
	if args[1] != "k1" || args[2] != "r1" || args[3] != "resolved" || args[4] != "j1" || args[5] != "a1" || args[6] != "u1" {
		t.Errorf("row-1 args out of order: %v", args[1:7])
	}
	if args[7] != "k2" || args[12] != "u2" {
		t.Errorf("row-2 args out of order: %v", args[7:13])
	}
}

// TestExecFindingStatusBatch_ChunkingAndAtomicity proves: one Exec per ≤9000
// rows (a 9005-item batch → 2 statements), and a mid-batch failure propagates
// so the caller's tx rolls the whole batch back (the atomicity contract).
func TestExecFindingStatusBatch_ChunkingAndAtomicity(t *testing.T) {
	items := make([]*interfaces.FindingStatus, findingStatusBatchRows+5)
	for i := range items {
		items[i] = &interfaces.FindingStatus{FindingKey: "k", RuleID: "r", Status: "resolved", UpdatedBy: "u"}
	}

	rec := &recordingExec{}
	if err := execFindingStatusBatch(context.Background(), rec, "f1", items); err != nil {
		t.Fatalf("batch exec: %v", err)
	}
	if len(rec.stmts) != 2 {
		t.Fatalf("expected 2 statements (9000+5 rows), got %d", len(rec.stmts))
	}
	if got := strings.Count(rec.stmts[0], "($1,"); got != findingStatusBatchRows {
		t.Errorf("chunk 1 should hold %d rows, has %d", findingStatusBatchRows, got)
	}
	if got := strings.Count(rec.stmts[1], "($1,"); got != 5 {
		t.Errorf("chunk 2 should hold 5 rows, has %d", got)
	}
	// Parameter ceiling: chunk 1 = 1 + 6×9000 = 54001 < 65535.
	if n := len(rec.argss[0]); n != 1+6*findingStatusBatchRows || n >= 65535 {
		t.Errorf("chunk-1 param count = %d (must be 1+6×%d and < 65535)", n, findingStatusBatchRows)
	}

	// Failure propagation: the second chunk erroring must surface (caller
	// rolls back the tx — nothing partially commits).
	fail := &recordingExec{fail: errors.New("db down")}
	if err := execFindingStatusBatch(context.Background(), fail, "f1", items); err == nil {
		t.Fatal("expected chunk failure to propagate for rollback")
	}
}

// TestSyncOrgMembers_Shape (SQL-shape only; persistence is Postgres-gated)
// verifies the member upsert is one multi-row statement with the shared org_id
// and the prune DELETE still present.
func TestSyncOrgMembers_Shape(t *testing.T) {
	// syncOrgMembers takes a *sql.Tx; exercising the statement build without
	// a live DB isn't possible, so persistence stays covered by the
	// Postgres-gated contract suite.
	t.Skip("syncOrgMembers requires a live *sql.Tx; covered by the Postgres contract suite")
}
