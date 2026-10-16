package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// explainBuffersAsUser runs EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) of q in a fresh RLS transaction for userID (rolled
// back), with the planner's own choices (real statistics, sequential scans allowed), and returns the shared buffers
// the statement touched, hit or read.
func explainBuffersAsUser(t *testing.T, b *PostgresStorageBackend, userID, q string, args []any) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := b.BeginRLS(ctx, userID)
	if err != nil {
		t.Fatalf("BeginRLS: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	var raw string
	if err := tx.QueryRowContext(ctx, "EXPLAIN (ANALYZE, BUFFERS, FORMAT JSON) "+q, args...).Scan(&raw); err != nil {
		t.Fatalf("EXPLAIN ANALYZE: %v\n%s", err, q)
	}
	var top []struct {
		Plan struct {
			Hit  int64 `json:"Shared Hit Blocks"`
			Read int64 `json:"Shared Read Blocks"`
		} `json:"Plan"`
	}
	if err := json.Unmarshal([]byte(raw), &top); err != nil || len(top) == 0 {
		t.Fatalf("parse explain json: %v\n%s", err, raw)
	}
	return top[0].Plan.Hit + top[0].Plan.Read
}

// tableReadsAsUser runs fn in a fresh RLS transaction for userID (rolled back), with the store's calls in fn scoped to
// it, and returns what the transaction's statements read while fn ran, whichever they were: the rows fetched and the
// index scans started, summed over every table (pg_stat_xact_user_tables counts them per transaction).
func tableReadsAsUser(t *testing.T, b *PostgresStorageBackend, userID string, fn func(ctx context.Context, tx *sql.Tx)) int64 {
	t.Helper()
	ctx := context.Background()
	tx, err := b.BeginRLS(ctx, userID)
	if err != nil {
		t.Fatalf("BeginRLS: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	reads := func() int64 {
		var n int64
		if err := tx.QueryRowContext(ctx, `SELECT COALESCE(sum(seq_tup_read + COALESCE(idx_tup_fetch, 0) + COALESCE(idx_scan, 0)), 0)::bigint
			FROM pg_stat_xact_user_tables`).Scan(&n); err != nil {
			t.Fatalf("read pg_stat_xact_user_tables: %v", err)
		}
		return n
	}
	before := reads()
	fn(WithRLSTx(ctx, tx), tx)
	return reads() - before
}

// TestPlan_LibraryKeysetPage_ReadsTheVisibleSetOnce: a caller who sees a small part of a large library (here 2008 of
// 42 000 flows) pays for every page with the whole visible set, whatever the page: the planner builds it (the
// visibility arms, a primary-key probe per flow with the RLS filter) and sorts it before the keyset bound or the
// LIMIT cuts. v49 counted it again in a second statement on every page but the last, twice the work of the pre-v49
// single statement with its window count. A keyset page now takes the total from its cursor and runs its rows'
// statement alone, at most what the pre-v49 statement cost; the first page still counts, once. Measured with
// EXPLAIN (ANALYZE, BUFFERS) on real statistics, as the app role under RLS, and for ListFlowsPage itself by the rows
// its transaction read.
func TestPlan_LibraryKeysetPage_ReadsTheVisibleSetOnce(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	seedLibraryPagePopulation(t, b)
	ctx := context.Background()
	for _, s := range []string{
		`INSERT INTO flows (id, name, content, owner_id, org_id, updated_at)
			SELECT 'plan-huge-' || g, 'Huge flow ' || g, '{}'::jsonb, 'plan-huge-owner-' || (g % 500), 'plan-org-huge-' || (g % 50),
			       NOW() - (g || ' seconds')::interval
			FROM generate_series(1, 38000) g`,
		`ANALYZE flows, flow_collaborators, org_members, organisations`,
	} {
		if _, err := b.db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	const user = planUser
	f := libraryPageFilter()

	// The pre-v49 page: the list statement with the window count riding every row.
	oldQ, oldArgs := listFlowsSQL(f)
	oldQ = strings.Replace(oldQ, "version, tags\n", "version, tags, COUNT(*) OVER() AS page_total\n", 1)
	old := explainBuffersAsUser(t, b, user, oldQ, oldArgs)

	pq, pargs := listFlowsPageSQL(f)
	cq, cargs := countFlowsCappedSQL(f, 10000)
	firstRows, firstCount := explainBuffersAsUser(t, b, user, pq, pargs), explainBuffersAsUser(t, b, user, cq, cargs)

	// Mid-library, as the next page's cursor names it.
	keyset := f
	keyset.After = &interfaces.PageCursor{At: time.Now().Add(-1000 * time.Minute), ID: "plan-big-1000"}
	kq, kargs := listFlowsPageSQL(keyset)
	kcq, kcargs := countFlowsCappedSQL(keyset, 10000)
	keysetRows, keysetCount := explainBuffersAsUser(t, b, user, kq, kargs), explainBuffersAsUser(t, b, user, kcq, kcargs)
	t.Logf("buffers: pre-v49 page %d; first page %d + count %d; keyset page %d (+ the count v49 also ran: %d)",
		old, firstRows, firstCount, keysetRows, keysetCount)

	// The count is not free: it reads what the page reads, so skipping it on a keyset page halves that page's work.
	if keysetCount*2 < keysetRows {
		t.Fatalf("the keyset count read %d buffers against the page's %d; the population does not show the double read", keysetCount, keysetRows)
	}
	if limit := old + old/4 + 50; keysetRows > limit {
		t.Errorf("a keyset page reads %d buffers, more than the pre-v49 single statement's %d (+25%%)", keysetRows, old)
	}

	// The store runs only the rows' statement for a cursor that carries the total: the page reports the cursor's, and
	// what ListFlowsPage reads is what that statement alone reads. A count beside it reads the visible set again.
	rowsOnly := tableReadsAsUser(t, b, user, func(ctx context.Context, tx *sql.Tx) {
		rows, err := tx.QueryContext(ctx, kq, kargs...)
		if err != nil {
			t.Fatalf("keyset rows: %v", err)
		}
		defer rows.Close()
		for rows.Next() {
		}
		if err := rows.Err(); err != nil {
			t.Fatalf("keyset rows: %v", err)
		}
	})
	countOnly := tableReadsAsUser(t, b, user, func(ctx context.Context, tx *sql.Tx) {
		var n int
		if err := tx.QueryRowContext(ctx, kcq, kcargs...).Scan(&n); err != nil {
			t.Fatalf("keyset count: %v", err)
		}
	})
	keyset.After.Counted, keyset.After.Total = true, 4242
	var page *interfaces.FlowPage
	var err error
	listed := tableReadsAsUser(t, b, user, func(ctx context.Context, _ *sql.Tx) { page, err = b.ListFlowsPage(ctx, keyset, 10000) })
	t.Logf("table reads: the keyset page's rows %d, its count %d; ListFlowsPage with a counted cursor %d", rowsOnly, countOnly, listed)
	if err != nil || page.Total != 4242 || len(page.Docs) != 25 || !page.HasMore {
		t.Fatalf("ListFlowsPage with a counted cursor = %+v, %v; want the cursor's total 4242 and a full page", page, err)
	}
	if countOnly*4 < rowsOnly {
		t.Fatalf("the keyset count read %d against the rows' %d; the population does not show the second read", countOnly, rowsOnly)
	}
	if listed > rowsOnly+rowsOnly/8 {
		t.Errorf("ListFlowsPage with a counted cursor read %d, its rows' statement alone %d: it ran another statement over the visible set", listed, rowsOnly)
	}
}
