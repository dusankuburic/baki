package database

import (
	"context"
	"strings"
	"testing"
	"time"
)

// perfRound3V28Checksum pins v28's SQL. Once v28 has shipped, a change to
// its text fails every deployed database's boot (verifyChecksums): fix
// forward with a v29 instead of editing v28.
const perfRound3V28Checksum = "9370145b1e9042c43cbe244e5dedb923072b945ef89ba3badc889775c2ea9166"

// v28 catalog objects: created by the up step, removed by the down step.
var (
	v28Indexes      = []string{"audit_events_action_created_idx", "knowledge_chunks_org_doc_idx"}
	v28DroppedIndex = "audit_events_action_idx"
	v28Trigger      = "knowledge_chunks_fill_org"
)

// v28ChunkPolicyBody is the rewritten rls_knowledge_chunks_visible body: the
// chunk's own org against the caller's memberships, no document probe.
const v28ChunkPolicyBody = "NOT app_rls_active() OR org_id IN (SELECT om.org_id FROM org_members om WHERE om.user_id = app_current_user_id())"

// TestMigration28_Shape pins the v28 step without a database: the composite
// action index replacing the single-column one, the org column with its
// backfill, index and fill trigger, the uncorrelated policy, no
// CONCURRENTLY — and a down step that restores the baseline policy before
// dropping the column it references.
func TestMigration28_Shape(t *testing.T) {
	m, ok := migrationByVersion(28)
	if !ok {
		t.Fatal("migration v28 not registered")
	}
	if m.name != "perf_round3_v28" {
		t.Errorf("v28 name = %q, want perf_round3_v28", m.name)
	}
	sqlText := oneLine(m.sql)
	for _, want := range []string{
		"CREATE INDEX IF NOT EXISTS audit_events_action_created_idx ON audit_events (action, created_at DESC, id ASC);",
		"DROP INDEX IF EXISTS audit_events_action_idx;",
		"ALTER TABLE knowledge_chunks ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT '';",
		"UPDATE knowledge_chunks c SET org_id = d.org_id FROM knowledge_documents d WHERE d.id = c.doc_id AND c.org_id = '';",
		"CREATE INDEX IF NOT EXISTS knowledge_chunks_org_doc_idx ON knowledge_chunks (org_id, doc_id, id);",
		"CREATE TRIGGER knowledge_chunks_fill_org BEFORE INSERT ON knowledge_chunks FOR EACH ROW EXECUTE FUNCTION knowledge_chunks_fill_org();",
		"DROP POLICY IF EXISTS rls_knowledge_chunks_visible ON knowledge_chunks;",
		"CREATE POLICY rls_knowledge_chunks_visible ON knowledge_chunks FOR ALL USING (" + v28ChunkPolicyBody + ") WITH CHECK (" + v28ChunkPolicyBody + ");",
	} {
		if !strings.Contains(sqlText, want) {
			t.Errorf("v28 lacks %q", want)
		}
	}
	if strings.Contains(strings.ToUpper(m.sql), "CONCURRENTLY") {
		t.Error("v28 must not use CONCURRENTLY: applyMigration runs each step in a transaction")
	}
	if strings.Count(strings.ToUpper(m.sql), "CREATE INDEX") != strings.Count(strings.ToUpper(m.sql), "CREATE INDEX IF NOT EXISTS") {
		t.Error("every v28 CREATE INDEX must be IF NOT EXISTS")
	}
	// The up policy must not probe the document per row.
	upBody := policyBody(sqlText, "rls_knowledge_chunks_visible", "knowledge_chunks")
	for _, banned := range []string{"knowledge_documents", "EXISTS", "doc_id"} {
		if strings.Contains(upBody, banned) {
			t.Errorf("v28 policy still probes the document per chunk row (%s): %q", banned, upBody)
		}
	}

	down := oneLine(m.downSQL)
	downBody := policyBody(down, "rls_knowledge_chunks_visible", "knowledge_chunks")
	// Exactly the baseline's correlated body (restored verbatim).
	for _, want := range []string{
		"SELECT 1 FROM knowledge_documents kd WHERE kd.id = knowledge_chunks.doc_id",
		"EXISTS (SELECT 1 FROM org_members om WHERE om.org_id = kd.org_id AND om.user_id = app_current_user_id())",
	} {
		if strings.Count(downBody, want) != 2 { // USING and WITH CHECK
			t.Errorf("v28 down does not restore the baseline chunk policy body (%q twice): %q", want, downBody)
		}
	}
	for _, want := range []string{
		"DROP TRIGGER IF EXISTS knowledge_chunks_fill_org ON knowledge_chunks;",
		"DROP FUNCTION IF EXISTS knowledge_chunks_fill_org();",
		"DROP INDEX IF EXISTS knowledge_chunks_org_doc_idx;",
		"ALTER TABLE knowledge_chunks DROP COLUMN IF EXISTS org_id;",
		"DROP INDEX IF EXISTS audit_events_action_created_idx;",
		"CREATE INDEX IF NOT EXISTS audit_events_action_idx ON audit_events (action);",
	} {
		if !strings.Contains(down, want) {
			t.Errorf("v28 down lacks %q", want)
		}
	}
	// The policy references org_id: it must be restored BEFORE the column
	// is dropped, or the DROP COLUMN fails on the dependency.
	if strings.Index(down, "CREATE POLICY rls_knowledge_chunks_visible") > strings.Index(down, "DROP COLUMN IF EXISTS org_id") {
		t.Error("v28 down must restore the policy before dropping org_id")
	}
}

// TestMigration28_ChecksumPinned fails when v28's SQL text changes.
func TestMigration28_ChecksumPinned(t *testing.T) {
	m, _ := migrationByVersion(28)
	if got := migrationChecksum(m.sql); got != perfRound3V28Checksum {
		t.Errorf("v28 checksum = %s, want the pinned %s — a shipped migration must never change; add v29 instead", got, perfRound3V28Checksum)
	}
}

// chunkOrgColumnExists reports whether knowledge_chunks.org_id exists.
func chunkOrgColumnExists(t *testing.T, b *PostgresStorageBackend) bool {
	t.Helper()
	var ok bool
	if err := b.db.QueryRowContext(context.Background(), `
		SELECT EXISTS (SELECT 1 FROM information_schema.columns
		               WHERE table_schema = current_schema() AND table_name = 'knowledge_chunks' AND column_name = 'org_id')`).Scan(&ok); err != nil {
		t.Fatalf("information_schema.columns: %v", err)
	}
	return ok
}

func triggerExists(t *testing.T, b *PostgresStorageBackend, table, name string) bool {
	t.Helper()
	var ok bool
	if err := b.db.QueryRowContext(context.Background(), `
		SELECT EXISTS (SELECT 1 FROM pg_trigger WHERE tgrelid = $1::regclass AND tgname = $2 AND NOT tgisinternal)`, table, name).Scan(&ok); err != nil {
		t.Fatalf("pg_trigger: %v", err)
	}
	return ok
}

// assertV28Applied checks the catalog for the v28 end state (or its absence).
func assertV28Applied(t *testing.T, b *PostgresStorageBackend, applied bool) {
	t.Helper()
	have := catalogIndexes(t, b, append(append([]string{}, v28Indexes...), v28DroppedIndex))
	for _, idx := range v28Indexes {
		if have[idx] != applied {
			t.Errorf("index %s exists = %v, want %v", idx, have[idx], applied)
		}
	}
	if have[v28DroppedIndex] == applied {
		t.Errorf("index %s exists = %v, want %v", v28DroppedIndex, have[v28DroppedIndex], !applied)
	}
	if got := chunkOrgColumnExists(t, b); got != applied {
		t.Errorf("knowledge_chunks.org_id exists = %v, want %v", got, applied)
	}
	if got := triggerExists(t, b, "knowledge_chunks", v28Trigger); got != applied {
		t.Errorf("trigger %s exists = %v, want %v", v28Trigger, got, applied)
	}
	pols := tablePolicies(t, b, "knowledge_chunks")
	if len(pols) != 1 || pols[0].name != "rls_knowledge_chunks_visible" || pols[0].cmd != "ALL" {
		t.Fatalf("knowledge_chunks policies = %+v, want the one FOR ALL rls_knowledge_chunks_visible", pols)
	}
	for _, body := range []string{pols[0].qual, pols[0].check} {
		if probesDoc := strings.Contains(body, "knowledge_documents"); probesDoc == applied {
			t.Errorf("chunk policy probes the document = %v, want %v: %q", probesDoc, !applied, body)
		}
		if usesOrg := strings.Contains(body, "org_id IN"); usesOrg != applied {
			t.Errorf("chunk policy tests org_id IN (memberships) = %v, want %v: %q", usesOrg, applied, body)
		}
	}
}

// TestMigration28_UpDownUp is the DATABASE_URL-gated round trip: v28 applied
// (indexes, column, trigger, policy, checksum recorded), rolled back
// (baseline policy and action index restored, column and trigger gone),
// re-applied — and the re-apply backfills the org of a chunk written while
// the column was absent.
func TestMigration28_UpDownUp(t *testing.T) {
	b, dsn := openIsolatedMigrationDB(t)
	ctx := context.Background()
	current, err := b.CurrentSchemaVersion(ctx)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	if current < 28 {
		t.Skipf("DB at v%d; another test may have rolled it back", current)
	}
	assertV28Applied(t, b, true)
	var sum string
	if err := b.db.QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE version = 28`).Scan(&sum); err != nil {
		t.Fatalf("read v28 checksum: %v", err)
	}
	if sum != perfRound3V28Checksum {
		t.Errorf("recorded v28 checksum = %s, want %s", sum, perfRound3V28Checksum)
	}

	// Down.
	rolled, err := b.MigrateDown(ctx, 27)
	if err != nil {
		t.Fatalf("MigrateDown(27): %v", err)
	}
	// Later rounds append migrations above v28; MigrateDown(27) rolls them
	// all back, newest first, and v28 must be the last one rolled.
	if len(rolled) == 0 || rolled[len(rolled)-1] != 28 {
		t.Fatalf("rolled back %v, want a list ending in 28", rolled)
	}
	assertV28Applied(t, b, false)

	// A chunk written at v27 (raw: no org column) must get its org from the
	// re-applied backfill.
	stamp := time.Now().Format("150405.000000000")
	orgID, docID, chunkID := "r3-mig-org-"+stamp, "r3-mig-doc-"+stamp, "r3-mig-chunk-"+stamp
	seedOrgRow(t, b.db, orgID) // v48, re-applied with it, deletes a document whose organisation does not exist
	if _, err := b.db.ExecContext(ctx, `INSERT INTO knowledge_documents (id, org_id, filename) VALUES ($1, $2, 'mig.txt')`, docID, orgID); err != nil {
		t.Fatalf("insert document at v27: %v", err)
	}
	t.Cleanup(func() { _, _ = b.db.ExecContext(ctx, `DELETE FROM knowledge_documents WHERE id = $1`, docID) })
	if _, err := b.db.ExecContext(ctx, `INSERT INTO knowledge_chunks (id, doc_id, content, embedding) VALUES ($1, $2, 'c', '[1,0,0]'::jsonb)`, chunkID, docID); err != nil {
		t.Fatalf("insert chunk at v27: %v", err)
	}

	// Up again: boot-time migrate on reconnect.
	b2, err := New(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("reconnect (re-apply v28): %v", err)
	}
	defer b2.Close()
	if v, _ := b2.CurrentSchemaVersion(ctx); v < 28 {
		t.Fatalf("after re-apply, version = %d, want >= 28", v)
	}
	assertV28Applied(t, b2, true)
	var gotOrg string
	if err := b2.db.QueryRowContext(ctx, `SELECT org_id FROM knowledge_chunks WHERE id = $1`, chunkID).Scan(&gotOrg); err != nil {
		t.Fatalf("read backfilled chunk: %v", err)
	}
	if gotOrg != orgID {
		t.Errorf("backfilled chunk org_id = %q, want %q", gotOrg, orgID)
	}
}
