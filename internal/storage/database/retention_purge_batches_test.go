package database

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"
)

// argInt reads an integer bound through the fake driver (whose
// CheckNamedValue keeps Go values as they are: int or int64).
func argInt(v driver.Value) int64 {
	switch n := v.(type) {
	case int:
		return int64(n)
	case int64:
		return n
	}
	return -1
}

// Tests for the batched retention purge (PurgeExpiredData) and the
// data-subject audit export page size (exportAuditEvents).

// r3ExecSQL is the fakesql driver with scripted row counts: fakeConn's
// ExecContext answers every statement with one affected row, which cannot
// express "this batch deleted nothing". affected decides per statement.
type r3ExecSQL struct {
	*fakeSQL
	affected func(query string, args []driver.NamedValue) (int64, error)
}

type r3Connector struct{ f *r3ExecSQL }

func (c r3Connector) Connect(context.Context) (driver.Conn, error) {
	c.f.mu.Lock()
	c.f.conns++
	id := c.f.conns
	c.f.mu.Unlock()
	return &r3Conn{fakeConn: &fakeConn{f: c.f.fakeSQL, id: id}, r: c.f}, nil
}

func (c r3Connector) Driver() driver.Driver { return fakeDriver{} }

type r3Conn struct {
	*fakeConn
	r *r3ExecSQL
}

func (c *r3Conn) ExecContext(_ context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	c.f.answer(c.fakeConn, query, args) // record it
	n, err := c.r.affected(query, args)
	if err != nil {
		return nil, err
	}
	return driver.RowsAffected(n), nil
}

// newR3ExecBackend returns a backend over the counting fake.
func newR3ExecBackend(t *testing.T, affected func(query string, args []driver.NamedValue) (int64, error)) (*PostgresStorageBackend, *fakeSQL) {
	t.Helper()
	f := &r3ExecSQL{fakeSQL: &fakeSQL{}, affected: affected}
	db := sql.OpenDB(r3Connector{f: f})
	t.Cleanup(func() { _ = db.Close() })
	return &PostgresStorageBackend{db: db}, f.fakeSQL
}

// deletesOf returns the recorded DELETE statements against table, in order.
func deletesOf(f *fakeSQL, table string) []fakeCall {
	var out []fakeCall
	for _, c := range f.Queries() {
		if strings.HasPrefix(c.query, "DELETE FROM "+table+" ") {
			out = append(out, c)
		}
	}
	return out
}

// TestPurgeExpiredData_BatchesUntilEmpty: each growth table is deleted in
// LIMIT purgeBatchSize batches by ctid, repeated until a batch removes
// nothing, with the rows summed per table; a failing table is recorded and
// the tables after it still run, the error being the join of every failure.
func TestPurgeExpiredData_BatchesUntilEmpty(t *testing.T) {
	old := purgeBatchSize
	purgeBatchSize = 7
	t.Cleanup(func() { purgeBatchSize = old })

	injected := errors.New("flow_analysis_history: canceling statement due to statement timeout")
	auditBatches := []int64{7, 7, 3, 0}
	auditCalls, usageCalls := 0, 0
	b, f := newR3ExecBackend(t, func(query string, args []driver.NamedValue) (int64, error) {
		switch {
		case strings.HasPrefix(query, "DELETE FROM audit_events "):
			n := auditBatches[auditCalls]
			auditCalls++
			return n, nil
		case strings.HasPrefix(query, "DELETE FROM flow_analysis_history "):
			return 0, injected
		case strings.HasPrefix(query, "DELETE FROM usage_metrics "):
			usageCalls++
			if usageCalls == 1 {
				return 2, nil
			}
			return 0, nil
		default:
			return 0, nil // every other table: nothing to purge (one statement each)
		}
	})

	res, err := b.PurgeExpiredData(context.Background(), 30)
	if !errors.Is(err, injected) {
		t.Fatalf("err = %v, want the injected flow_analysis_history failure joined in", err)
	}
	if !strings.Contains(err.Error(), "flow_analysis_history") {
		t.Errorf("error does not name the failing table: %v", err)
	}
	if res == nil {
		t.Fatal("result must be returned with the partial counts")
	}
	if res.AuditEvents != 17 || res.UsageMetrics != 2 || res.FlowAnalysisHistory != 0 || res.GovAlerts != 0 {
		t.Errorf("counts = audit %d, usage %d, history %d, alerts %d; want 17, 2, 0, 0", res.AuditEvents, res.UsageMetrics, res.FlowAnalysisHistory, res.GovAlerts)
	}

	// audit_events: four statements (7, 7, 3, then the empty one that ends
	// the loop), each the batched ctid form with the window and the batch
	// size bound.
	audit := deletesOf(f, "audit_events")
	if len(audit) != 4 {
		t.Fatalf("audit_events deletes = %d, want 4 (three batches and the empty one):\n%s", len(audit), describeCalls(audit))
	}
	for _, c := range audit {
		q := oneLine(c.query)
		if !strings.Contains(q, "WHERE ctid = ANY(ARRAY(SELECT ctid FROM audit_events WHERE created_at < NOW() - ($1 * INTERVAL '1 day') LIMIT $2))") {
			t.Errorf("audit_events delete is not the batched ctid form: %s", q)
		}
		if len(c.args) != 2 || argInt(c.args[0].Value) != 30 || argInt(c.args[1].Value) != 7 {
			t.Errorf("audit_events delete args = %v, want [30 7]", c.args)
		}
	}
	// The failure did not stop the sweep: usage_metrics (2, then 0) and
	// gov_alerts (0) still ran after flow_analysis_history failed.
	if n := len(deletesOf(f, "flow_analysis_history")); n != 1 {
		t.Errorf("flow_analysis_history deletes = %d, want 1 (the failing one)", n)
	}
	if n := len(deletesOf(f, "usage_metrics")); n != 2 {
		t.Errorf("usage_metrics deletes = %d, want 2 (one batch and the empty one)", n)
	}
	if n := len(deletesOf(f, "gov_alerts")); n != 1 {
		t.Errorf("gov_alerts deletes = %d, want 1", n)
	}
	// token_blacklist is a growth table too: batched, independent of the
	// retention window (no $1 window parameter).
	if bl := deletesOf(f, "token_blacklist"); len(bl) != 1 || !strings.Contains(bl[0].query, "ctid = ANY(ARRAY(SELECT ctid FROM token_blacklist WHERE expires_at < NOW() LIMIT $1))") {
		t.Errorf("token_blacklist delete is not the batched form:\n%s", describeCalls(bl))
	}
	// Sessions are a growth table too (a row per sign-in and per refresh), and so are the rule notices (a row per
	// scope and rule set, the retention window): batched.
	if rt := deletesOf(f, "refresh_tokens"); len(rt) != 1 || !strings.Contains(rt[0].query, "ctid = ANY(ARRAY(SELECT ctid FROM refresh_tokens WHERE expires_at < NOW() LIMIT $1))") {
		t.Errorf("refresh_tokens delete is not the batched form:\n%s", describeCalls(rt))
	}
	if rn := deletesOf(f, "rule_notices"); len(rn) != 1 || !strings.Contains(rn[0].query, "ctid = ANY(ARRAY(SELECT ctid FROM rule_notices WHERE updated_at < NOW() - ($1 * INTERVAL '1 day') LIMIT $2))") {
		t.Errorf("rule_notices delete is not the batched form:\n%s", describeCalls(rn))
	}
	// The small tables stay single statements.
	for _, table := range []string{"api_tokens", "user_tokens", "org_invites", "share_tokens", "org_ci_credentials"} {
		d := deletesOf(f, table)
		if len(d) != 1 || strings.Contains(d[0].query, "ctid") {
			t.Errorf("%s: want one plain delete, got:\n%s", table, describeCalls(d))
		}
	}
}

// TestPurgeExpiredData_StopsWhenContextEnds: a context that ends between
// batches stops the sweep (no statement is sent against a dead context),
// the rows removed so far are reported, and the error carries the cause.
func TestPurgeExpiredData_StopsWhenContextEnds(t *testing.T) {
	old := purgeBatchSize
	purgeBatchSize = 5
	t.Cleanup(func() { purgeBatchSize = old })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	b, f := newR3ExecBackend(t, func(query string, _ []driver.NamedValue) (int64, error) {
		if strings.HasPrefix(query, "DELETE FROM audit_events ") {
			cancel() // the sweep's deadline passes after this batch
			return 5, nil
		}
		return 0, nil
	})
	res, err := b.PurgeExpiredData(ctx, 30)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
	if res.AuditEvents != 5 {
		t.Errorf("AuditEvents = %d, want the 5 removed before the context ended", res.AuditEvents)
	}
	if n := len(deletesOf(f, "audit_events")); n != 1 {
		t.Errorf("audit_events deletes = %d, want 1 (no statement after cancellation)", n)
	}
	for _, table := range []string{"flow_analysis_history", "usage_metrics", "gov_alerts"} {
		if n := len(deletesOf(f, table)); n != 0 {
			t.Errorf("%s was swept after the context ended (%d statements)", table, n)
		}
	}
}

// TestPurgeExpiredData_NoRetentionSkipsAgedTables: auditRetentionDays <= 0
// keeps the age-based tables untouched (the token_blacklist sweep is not
// age-based and still runs).
func TestPurgeExpiredData_NoRetentionSkipsAgedTables(t *testing.T) {
	b, f := newR3ExecBackend(t, func(string, []driver.NamedValue) (int64, error) { return 0, nil })
	if _, err := b.PurgeExpiredData(context.Background(), 0); err != nil {
		t.Fatalf("PurgeExpiredData: %v", err)
	}
	for _, table := range []string{"audit_events", "flow_analysis_history", "usage_metrics", "gov_alerts"} {
		if n := len(deletesOf(f, table)); n != 0 {
			t.Errorf("%s swept with no retention window (%d statements)", table, n)
		}
	}
	if n := len(deletesOf(f, "token_blacklist")); n != 1 {
		t.Errorf("token_blacklist deletes = %d, want 1", n)
	}
}

func describeCalls(calls []fakeCall) string {
	var sb strings.Builder
	for _, c := range calls {
		fmt.Fprintf(&sb, "  %s %v\n", oneLine(c.query), c.args)
	}
	return sb.String()
}

// TestPurgeExpiredData_DB_BatchesClearTheBacklog is the DATABASE_URL-gated
// counterpart: 3×batch+1 aged audit rows (batch shrunk to 7) are all
// removed, live rows are untouched, and the count is exact.
func TestPurgeExpiredData_DB_BatchesClearTheBacklog(t *testing.T) {
	b := openMigrationTestDB(t)
	ctx := context.Background()
	old := purgeBatchSize
	purgeBatchSize = 7
	t.Cleanup(func() { purgeBatchSize = old })

	stamp := time.Now().Format("150405.000000000")
	prefix := "r3-purge-" + stamp + "-"
	t.Cleanup(func() { _, _ = b.db.ExecContext(ctx, `DELETE FROM audit_events WHERE id LIKE $1`, prefix+"%") })
	const aged, live = 3*7 + 1, 3
	if _, err := b.db.ExecContext(ctx, `
		INSERT INTO audit_events (id, user_id, action, created_at)
		SELECT $1 || 'aged-' || g, 'r3-purge', 'test', NOW() - INTERVAL '100 days' - (g || ' seconds')::interval
		FROM generate_series(1, $2::int) g`, prefix, aged); err != nil {
		t.Fatalf("seed aged: %v", err)
	}
	if _, err := b.db.ExecContext(ctx, `
		INSERT INTO audit_events (id, user_id, action, created_at)
		SELECT $1 || 'live-' || g, 'r3-purge', 'test', NOW() - (g || ' days')::interval
		FROM generate_series(1, $2::int) g`, prefix, live); err != nil {
		t.Fatalf("seed live: %v", err)
	}
	// Whatever else the (shared) database holds past the window is purged
	// too; the count must equal the whole aged population.
	var agedTotal int
	if err := b.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE created_at < NOW() - INTERVAL '30 days'`).Scan(&agedTotal); err != nil {
		t.Fatal(err)
	}
	if agedTotal < aged {
		t.Fatalf("aged population %d < seeded %d", agedTotal, aged)
	}

	res, err := b.PurgeExpiredData(ctx, 30)
	if err != nil {
		t.Fatalf("PurgeExpiredData: %v", err)
	}
	if res.AuditEvents != agedTotal {
		t.Errorf("AuditEvents = %d, want %d (every aged row, across %d batches)", res.AuditEvents, agedTotal, (agedTotal+6)/7)
	}
	var agedLeft, liveLeft int
	if err := b.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE created_at < NOW() - INTERVAL '30 days'`).Scan(&agedLeft); err != nil {
		t.Fatal(err)
	}
	if err := b.db.QueryRowContext(ctx, `SELECT count(*) FROM audit_events WHERE id LIKE $1`, prefix+"live-%").Scan(&liveLeft); err != nil {
		t.Fatal(err)
	}
	if agedLeft != 0 {
		t.Errorf("%d aged audit rows survive the purge", agedLeft)
	}
	if liveLeft != live {
		t.Errorf("%d live audit rows left, want %d untouched", liveLeft, live)
	}
}

// TestExportAuditEvents_DB_CarriesEveryEvent seeds more than one clamp's
// worth of events for one user on the throwaway DB and asserts the export
// walks them all.
func TestExportAuditEvents_DB_CarriesEveryEvent(t *testing.T) {
	if os.Getenv("DATABASE_URL") == "" {
		t.Skip("DATABASE_URL not set")
	}
	b := openMigrationTestDB(t)
	ctx := context.Background()
	stamp := time.Now().Format("150405.000000000")
	user := "r3-export-" + stamp
	t.Cleanup(func() { _, _ = b.db.ExecContext(ctx, `DELETE FROM audit_events WHERE user_id = $1`, user) })
	const total = maxAuditLimit + 7
	if _, err := b.db.ExecContext(ctx, `
		INSERT INTO audit_events (id, user_id, action, created_at)
		SELECT $1 || '-' || g, $1, 'test', NOW() - (g || ' seconds')::interval
		FROM generate_series(1, $2::int) g`, user, total); err != nil {
		t.Fatalf("seed: %v", err)
	}
	got, err := b.exportAuditEvents(ctx, user)
	if err != nil {
		t.Fatalf("exportAuditEvents: %v", err)
	}
	if len(got) != total {
		t.Fatalf("export carries %d audit events, want all %d", len(got), total)
	}
	seen := map[string]bool{}
	for _, ev := range got {
		if seen[ev.ID] {
			t.Errorf("event %s exported twice", ev.ID)
		}
		seen[ev.ID] = true
	}
}

// TestPurgeExpiredData_DB_RefreshTokensInBatches: the expired sessions go in batches (shrunk to 7) until none is
// left, the live ones stay, and the count is exact.
func TestPurgeExpiredData_DB_RefreshTokensInBatches(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	old := purgeBatchSize
	purgeBatchSize = 7
	t.Cleanup(func() { purgeBatchSize = old })

	const user = "purge-rt-user"
	if _, err := b.db.ExecContext(ctx, `INSERT INTO users (id, email, password, role) VALUES ($1, $1 || '@example.com', 'x', 'member')`, user); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	const expired, live = 3*7 + 1, 3
	if _, err := b.db.ExecContext(ctx, `
		INSERT INTO refresh_tokens (jti, user_id, expires_at)
		SELECT 'expired-' || g, $1, NOW() - (g || ' minutes')::interval FROM generate_series(1, $2::int) g
		UNION ALL
		SELECT 'live-' || g, $1, NOW() + (g || ' hours')::interval FROM generate_series(1, $3::int) g`, user, expired, live); err != nil {
		t.Fatalf("seed sessions: %v", err)
	}
	res, err := b.PurgeExpiredData(ctx, 0)
	if err != nil {
		t.Fatalf("PurgeExpiredData: %v", err)
	}
	if res.RefreshTokens != expired {
		t.Errorf("RefreshTokens = %d, want %d (every expired session, across %d batches)", res.RefreshTokens, expired, (expired+6)/7)
	}
	var left, liveLeft int
	if err := b.db.QueryRowContext(ctx, `SELECT count(*) FILTER (WHERE expires_at < NOW()), count(*) FILTER (WHERE jti LIKE 'live-%') FROM refresh_tokens`).Scan(&left, &liveLeft); err != nil {
		t.Fatal(err)
	}
	if left != 0 || liveLeft != live {
		t.Errorf("after the purge: %d expired and %d live sessions; want 0 and %d", left, liveLeft, live)
	}
}
