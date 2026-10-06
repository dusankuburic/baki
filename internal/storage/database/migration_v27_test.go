package database

import (
	"context"
	"strings"
	"testing"
	"time"
)

// perfRound2V27Checksum pins v27's SQL. Once v27 has shipped, a change to
// its text fails every deployed database's boot (verifyChecksums): fix
// forward with a v28 instead of editing v27.
const perfRound2V27Checksum = "aab5cf6d5ee492a73b665a1af28d7fc110d721d03855ca2c62d517650bb2d33f"

// v27Indexes are the indexes v27 creates (and its down step drops).
var v27Indexes = []string{
	"flow_search_name_trgm_idx",
	"flow_search_body_trgm_idx",
	"gov_alerts_flow_created_idx",
	"audit_events_resource_created_idx",
	"usage_metrics_created_brin",
	"knowledge_documents_org_filename_idx",
}

// v27DroppedIndexes are the two trigram indexes on flows v27 drops (unusable
// by the app role) and its down step recreates.
var v27DroppedIndexes = []string{"flows_name_trgm_idx", "flows_content_trgm_idx"}

// v27DelegatedPolicies are the seven flow-child policies v27 rewrites to
// delegate to rls_flows_visible (and its down step restores). forClause is
// the exact text between the table name and USING, so a policy created
// without FOR (finding_comments_modify, v2) keeps that shape.
var v27DelegatedPolicies = []struct{ table, name, forClause string }{
	{"finding_comments", "finding_comments_modify", ""},
	{"conversations", "rls_conversations_visible", "FOR ALL "},
	{"flow_versions", "rls_flow_versions_visible", "FOR ALL "},
	{"flow_analysis_history", "rls_flow_analysis_history_visible", "FOR ALL "},
	{"finding_status", "rls_finding_status_visible", "FOR ALL "},
	{"flow_baselines", "rls_flow_baselines_visible", "FOR ALL "},
	{"gov_alerts", "rls_gov_alerts_visible", "FOR ALL "},
}

// policyBody returns the text of `CREATE POLICY name ON table ...;` in a
// one-lined SQL script, or "" when absent.
func policyBody(oneLined, name, table string) string {
	head := "CREATE POLICY " + name + " ON " + table + " "
	i := strings.Index(oneLined, head)
	if i < 0 {
		return ""
	}
	rest := oneLined[i+len(head):]
	if j := strings.Index(rest, ");"); j >= 0 {
		rest = rest[:j+2]
	}
	return rest
}

// TestMigration27_Shape pins the v27 step without a database: the side
// table with its cascade, no RLS on it, the backfill, the index set, no
// CONCURRENTLY, and a down step that reverses exactly that.
func TestMigration27_Shape(t *testing.T) {
	m, ok := migrationByVersion(27)
	if !ok {
		t.Fatal("migration v27 not registered")
	}
	if m.name != "perf_round2_v27" {
		t.Errorf("v27 name = %q, want perf_round2_v27", m.name)
	}
	sqlText := oneLine(m.sql)
	for _, want := range []string{
		"CREATE TABLE IF NOT EXISTS flow_search ( flow_id TEXT PRIMARY KEY REFERENCES flows(id) ON DELETE CASCADE, name TEXT NOT NULL DEFAULT '', body TEXT NOT NULL DEFAULT '' );",
		"CREATE INDEX IF NOT EXISTS flow_search_name_trgm_idx ON flow_search USING gin (name gin_trgm_ops);",
		"CREATE INDEX IF NOT EXISTS flow_search_body_trgm_idx ON flow_search USING gin (body gin_trgm_ops);",
		"INSERT INTO flow_search (flow_id, name, body) SELECT id, name, content::text FROM flows ON CONFLICT (flow_id) DO NOTHING;",
		"DROP INDEX IF EXISTS flows_content_trgm_idx;",
		"DROP INDEX IF EXISTS flows_name_trgm_idx;",
		"CREATE INDEX IF NOT EXISTS gov_alerts_flow_created_idx ON gov_alerts (flow_id, created_at DESC) WHERE dismissed_at IS NULL;",
		"CREATE INDEX IF NOT EXISTS audit_events_resource_created_idx ON audit_events (resource_type, resource_id, created_at DESC);",
		"CREATE INDEX IF NOT EXISTS usage_metrics_created_brin ON usage_metrics USING brin (created_at);",
		"CREATE INDEX IF NOT EXISTS knowledge_documents_org_filename_idx ON knowledge_documents (org_id, filename);",
	} {
		if !strings.Contains(sqlText, want) {
			t.Errorf("v27 lacks %q", want)
		}
	}
	if strings.Contains(sqlText, "ALTER TABLE flow_search") || strings.Contains(sqlText, "ON flow_search FOR ") || strings.Contains(sqlText, "ON flow_search USING (") {
		t.Error("flow_search must carry NO row-level security: the trigram indexes are only usable without it, and the outer read of flows keeps results under RLS")
	}
	if strings.Contains(strings.ToUpper(m.sql), "CONCURRENTLY") {
		t.Error("v27 must not use CONCURRENTLY: applyMigration runs each step in a transaction")
	}
	if strings.Count(strings.ToUpper(m.sql), "CREATE INDEX") != strings.Count(strings.ToUpper(m.sql), "CREATE INDEX IF NOT EXISTS") {
		t.Error("every v27 CREATE INDEX must be IF NOT EXISTS")
	}
	down := oneLine(m.downSQL)
	// The seven flow-child policies delegate to rls_flows_visible: each is
	// dropped and recreated under its own name, the new body names only
	// flows (never flow_collaborators / org_members / owner_id), and the
	// down step restores a body that does repeat the three-way check.
	for _, p := range v27DelegatedPolicies {
		drop := "DROP POLICY IF EXISTS " + p.name + " ON " + p.table + ";"
		if strings.Count(sqlText, drop) != 1 {
			t.Errorf("v27 must drop %s on %s exactly once", p.name, p.table)
		}
		body := "NOT app_rls_active() OR EXISTS (SELECT 1 FROM flows f WHERE f.id = " + p.table + ".flow_id)"
		create := "CREATE POLICY " + p.name + " ON " + p.table + " " + p.forClause + "USING (" + body + ") WITH CHECK (" + body + ");"
		if !strings.Contains(sqlText, create) {
			t.Errorf("v27 lacks the delegating policy %q", create)
		}
		downBody := policyBody(down, p.name, p.table)
		if !strings.Contains(downBody, "flow_collaborators fc") || !strings.Contains(downBody, "org_members om") || !strings.Contains(downBody, "f.owner_id = app_current_user_id()") {
			t.Errorf("v27 down does not restore the original three-way body of %s on %s: %q", p.name, p.table, downBody)
		}
	}
	upPolicies := strings.Split(sqlText, "CREATE POLICY ")[1:]
	if len(upPolicies) != len(v27DelegatedPolicies) {
		t.Errorf("v27 creates %d policies, want %d", len(upPolicies), len(v27DelegatedPolicies))
	}
	for _, body := range upPolicies {
		body = body[:strings.Index(body, ";")]
		for _, banned := range []string{"flow_collaborators", "org_members", "owner_id"} {
			if strings.Contains(body, banned) {
				t.Errorf("v27 policy repeats the flows visibility check (%s): %q", banned, body)
			}
		}
	}
	for _, idx := range v27Indexes {
		if strings.HasPrefix(idx, "flow_search_") {
			continue // go with DROP TABLE flow_search (checked against the catalog in TestMigration27_UpDownUp)
		}
		if !strings.Contains(down, "DROP INDEX IF EXISTS "+idx+";") {
			t.Errorf("v27 down does not drop %s", idx)
		}
	}
	for _, want := range []string{
		"DROP TABLE IF EXISTS flow_search;",
		// Exactly the v13 / v18 definitions.
		"CREATE INDEX IF NOT EXISTS flows_name_trgm_idx ON flows USING gin (name gin_trgm_ops);",
		"CREATE INDEX IF NOT EXISTS flows_content_trgm_idx ON flows USING gin ((content::text) gin_trgm_ops);",
	} {
		if !strings.Contains(down, want) {
			t.Errorf("v27 down lacks %q", want)
		}
	}
}

// TestMigration27_ChecksumPinned fails when v27's SQL text changes.
func TestMigration27_ChecksumPinned(t *testing.T) {
	m, _ := migrationByVersion(27)
	if got := migrationChecksum(m.sql); got != perfRound2V27Checksum {
		t.Errorf("v27 checksum = %s, want the pinned %s — a shipped migration must never change; add v28 instead", got, perfRound2V27Checksum)
	}
}

// catalogIndexes returns which of names exist in the current schema.
func catalogIndexes(t *testing.T, b *PostgresStorageBackend, names []string) map[string]bool {
	t.Helper()
	rows, err := b.db.QueryContext(context.Background(),
		`SELECT indexname FROM pg_indexes WHERE schemaname = current_schema() AND indexname = ANY($1)`, names)
	if err != nil {
		t.Fatalf("pg_indexes: %v", err)
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var n string
		if err := rows.Scan(&n); err != nil {
			t.Fatalf("scan: %v", err)
		}
		out[n] = true
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pg_indexes rows: %v", err)
	}
	return out
}

func tableExists(t *testing.T, b *PostgresStorageBackend, name string) bool {
	t.Helper()
	var ok bool
	if err := b.db.QueryRowContext(context.Background(),
		`SELECT EXISTS (SELECT 1 FROM pg_tables WHERE schemaname = current_schema() AND tablename = $1)`, name).Scan(&ok); err != nil {
		t.Fatalf("pg_tables: %v", err)
	}
	return ok
}

// assertV27Applied checks the catalog for the v27 end state.
func assertV27Applied(t *testing.T, b *PostgresStorageBackend, applied bool) {
	t.Helper()
	if got := tableExists(t, b, "flow_search"); got != applied {
		t.Errorf("flow_search exists = %v, want %v", got, applied)
	}
	have := catalogIndexes(t, b, append(append([]string{}, v27Indexes...), v27DroppedIndexes...))
	for _, idx := range v27Indexes {
		if have[idx] != applied {
			t.Errorf("index %s exists = %v, want %v", idx, have[idx], applied)
		}
	}
	for _, idx := range v27DroppedIndexes {
		if have[idx] == applied {
			t.Errorf("index %s exists = %v, want %v", idx, have[idx], !applied)
		}
	}
}

// TestMigration27_UpDownUp is the DATABASE_URL-gated round trip: v27
// applied (table, indexes, the two flows trigram indexes gone, no RLS on the
// side table, checksum recorded), rolled back (table gone, trigram indexes
// restored, v27's indexes gone), re-applied — and the re-apply backfills a
// flow written while the side table was absent.
func TestMigration27_UpDownUp(t *testing.T) {
	b, dsn := openIsolatedMigrationDB(t)
	ctx := context.Background()
	current, err := b.CurrentSchemaVersion(ctx)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	if current < 27 {
		t.Skipf("DB at v%d; another test may have rolled it back", current)
	}
	assertV27Applied(t, b, true)

	var rls, forced bool
	if err := b.db.QueryRowContext(ctx, `SELECT relrowsecurity, relforcerowsecurity FROM pg_class WHERE relname = 'flow_search' AND relnamespace = current_schema()::regnamespace`).Scan(&rls, &forced); err != nil {
		t.Fatalf("pg_class flow_search: %v", err)
	}
	if rls || forced {
		t.Errorf("flow_search has row-level security (enabled=%v forced=%v); it must have none", rls, forced)
	}
	var sum string
	if err := b.db.QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE version = 27`).Scan(&sum); err != nil {
		t.Fatalf("read v27 checksum: %v", err)
	}
	if sum != perfRound2V27Checksum {
		t.Errorf("recorded v27 checksum = %s, want %s", sum, perfRound2V27Checksum)
	}

	// Down.
	rolled, err := b.MigrateDown(ctx, 26)
	if err != nil {
		t.Fatalf("MigrateDown(26): %v", err)
	}
	// Later rounds append migrations above v27; MigrateDown(26) rolls them
	// all back, newest first, and v27 must be the last one rolled.
	if len(rolled) == 0 || rolled[len(rolled)-1] != 27 {
		t.Fatalf("rolled back %v, want a list ending in 27", rolled)
	}
	assertV27Applied(t, b, false)

	// A flow written at v26 (raw insert: SaveFlow needs the side table) must
	// be picked up by the re-applied backfill.
	flowID := "r2-mig-" + time.Now().Format("150405.000000000")
	if _, err := b.db.ExecContext(ctx,
		`INSERT INTO flows (id, name, content, owner_id) VALUES ($1, 'Backfilled', '{"k": "backfill-needle"}'::jsonb, 'r2-mig-owner')`, flowID); err != nil {
		t.Fatalf("insert flow at v26: %v", err)
	}
	t.Cleanup(func() { _, _ = b.db.ExecContext(ctx, `DELETE FROM flows WHERE id = $1`, flowID) })

	// Up again: boot-time migrate on reconnect.
	b2, err := New(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("reconnect (re-apply v27): %v", err)
	}
	defer b2.Close()
	if v, _ := b2.CurrentSchemaVersion(ctx); v < 27 {
		t.Fatalf("after re-apply, version = %d, want >= 27", v)
	}
	assertV27Applied(t, b2, true)
	var name, body string
	if err := b2.db.QueryRowContext(ctx, `SELECT name, body FROM flow_search WHERE flow_id = $1`, flowID).Scan(&name, &body); err != nil {
		t.Fatalf("backfilled flow_search row: %v", err)
	}
	if name != "Backfilled" || !strings.Contains(body, "backfill-needle") {
		t.Errorf("backfill wrote name=%q body=%q", name, body)
	}
}
