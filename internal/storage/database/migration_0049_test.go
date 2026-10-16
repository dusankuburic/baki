package database

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"
)

// indexes0049 are the indexes of v49, as its statements read: the two the step builds (the shape test pins them to
// the step) and the three of users that SchemaUpkeep builds after boot (without CONCURRENTLY here; the shape test pins
// them to the upkeep). The plan tests create them on a private database, so they hold whether the schema they start
// from has them or not.
var indexes0049 = map[string]string{
	"knowledge_documents_org_created_idx": `CREATE INDEX IF NOT EXISTS knowledge_documents_org_created_idx ON knowledge_documents (org_id, created_at DESC, id);`,
	"users_email_trgm_idx":                `CREATE INDEX IF NOT EXISTS users_email_trgm_idx ON users USING gin (lower(email) gin_trgm_ops);`,
	"users_display_name_trgm_idx":         `CREATE INDEX IF NOT EXISTS users_display_name_trgm_idx ON users USING gin (lower(display_name) gin_trgm_ops);`,
	"users_locked_until_idx":              `CREATE INDEX IF NOT EXISTS users_locked_until_idx ON users (locked_until) WHERE locked_until IS NOT NULL;`,
	"rule_notices_app_idx":                `CREATE INDEX IF NOT EXISTS rule_notices_app_idx ON rule_notices (id) WHERE scope = 'app';`,
}

// stepIndexes0049 are the ones the step builds itself, on small tables written rarely.
var stepIndexes0049 = []string{"knowledge_documents_org_created_idx", "rule_notices_app_idx"}

// dropped0049 are the indexes v49 retires (SchemaUpkeep drops them concurrently after boot), with the statement its
// down step restores each with (the HNSW index over the dimensionless column was never built, so nothing restores it).
var dropped0049 = map[string]string{
	"knowledge_docs_org_idx":          `CREATE INDEX IF NOT EXISTS knowledge_docs_org_idx ON knowledge_documents (org_id);`,
	"org_custom_rules_org_idx":        `CREATE INDEX IF NOT EXISTS org_custom_rules_org_idx ON org_custom_rules (org_id);`,
	"audit_events_org_created_idx":    `CREATE INDEX IF NOT EXISTS audit_events_org_created_idx ON audit_events (org_id, created_at DESC, id ASC) WHERE org_id IS NOT NULL;`,
	"knowledge_chunks_embedding_hnsw": ``,
}

// TestMigration0049_Shape pins the step without a database: expand, only the two builds on small tables (no lock on
// users or audit_events, no drop, whose lock blocks reads and queues the table's queries while it waits), the users
// indexes and the drops in SchemaUpkeep's concurrent work instead, nothing UNIQUE, and a down step that reverses it.
func TestMigration0049_Shape(t *testing.T) {
	m, ok := migrationByVersion(49)
	if !ok {
		t.Fatal("migration v49 not registered")
	}
	if m.name != "index_upkeep" || m.compat != compatExpand {
		t.Errorf("v49 = %q (%v), want index_upkeep, expand (it only builds and drops non-unique indexes)", m.name, m.compat)
	}
	up, down := oneLine(m.sql), oneLine(m.downSQL)
	upkeep := map[string]concurrentIndex{}
	for _, ix := range schemaUpkeepIndexes {
		if ix.since == 49 {
			upkeep[ix.name] = ix
		}
	}
	for name, stmt := range indexes0049 {
		if slices.Contains(stepIndexes0049, name) {
			if !strings.Contains(up, stmt) {
				t.Errorf("v49 lacks %q", stmt)
			}
		} else {
			want := strings.TrimSuffix(strings.Replace(stmt, "CREATE INDEX IF NOT EXISTS", "CREATE INDEX CONCURRENTLY IF NOT EXISTS", 1), ";")
			if ix, ok := upkeep[name]; !ok || ix.ddl != want {
				t.Errorf("SchemaUpkeep builds %s as %q, want %q", name, ix.ddl, want)
			}
			if strings.Contains(up, name) {
				t.Errorf("v49 itself builds %s; its build blocks writes to users until the step commits", name)
			}
		}
		if !strings.Contains(down, "DROP INDEX IF EXISTS "+name+";") {
			t.Errorf("v49 down does not drop %s", name)
		}
	}
	for name, restore := range dropped0049 {
		if ix, ok := upkeep[name]; !ok || ix.ddl != "" {
			t.Errorf("SchemaUpkeep does not drop %s concurrently: %+v", name, ix)
		}
		if restore != "" && !strings.Contains(down, restore) {
			t.Errorf("v49 down lacks %q", restore)
		}
	}
	// The retirement of knowledge_docs_org_idx comes after the step built its replacement, which runs before boot.
	upper := strings.ToUpper(strings.Join(stepStatements(m.sql), " ")) // the statements, not the comments
	for _, forbidden := range []string{"DROP INDEX", "ON USERS", "AUDIT_EVENTS", "CONCURRENTLY", "CREATE UNIQUE"} {
		if strings.Contains(upper, forbidden) {
			t.Errorf("v49 must not contain %s", forbidden)
		}
	}
}

// TestMigration0049_LocksOnlyItsTwoSmallTables: the step runs while the release before serves and holds its locks to
// its commit, and a lock it waits for holds up every later query on that table. It used to build on users (SHARE:
// sign-ins wait) and drop on audit_events (ACCESS EXCLUSIVE, queued behind any open reader of the audit log, every
// audit write queued behind it) while it held knowledge_documents and org_custom_rules exclusively. Now it locks only
// knowledge_documents and rule_notices beyond reading them; pg_locks after each statement, in one transaction.
func TestMigration0049_LocksOnlyItsTwoSmallTables(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	if rolled, err := b.MigrateDown(ctx, 48); err != nil || len(rolled) == 0 || rolled[len(rolled)-1] != 49 {
		t.Fatalf("MigrateDown(48) = %v, %v", rolled, err)
	}
	m, _ := migrationByVersion(49)
	conn, err := b.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	allowed := []string{"knowledge_documents", "rule_notices"}
	lockedBeyond := func() (locked []string) {
		t.Helper()
		rows, err := tx.QueryContext(ctx, `SELECT DISTINCT c.relname FROM pg_locks l JOIN pg_class c ON c.oid = l.relation
			WHERE l.pid = pg_backend_pid() AND l.locktype = 'relation' AND c.relkind = 'r'
			  AND c.relnamespace = current_schema()::regnamespace AND l.mode <> 'AccessShareLock'`)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		for rows.Next() {
			var name string
			if err := rows.Scan(&name); err != nil {
				t.Fatal(err)
			}
			if !slices.Contains(allowed, name) {
				locked = append(locked, name)
			}
		}
		if err := rows.Err(); err != nil {
			t.Fatal(err)
		}
		return locked
	}
	for i, s := range stepStatements(m.sql) {
		if _, err := tx.ExecContext(ctx, s); err != nil {
			t.Fatalf("statement %d: %v\n%s", i+1, err, s)
		}
		if locked := lockedBeyond(); len(locked) > 0 {
			t.Errorf("statement %d locks %v until the step commits:\n%s", i+1, locked, s)
		}
	}
}

// v49Indexes reports, at the schema b is on, how many of v49's new indexes exist valid and how many of the ones it
// retires do.
func v49Indexes(t *testing.T, ctx context.Context, b *PostgresStorageBackend) (built, dropped int) {
	t.Helper()
	for name := range indexes0049 {
		if validIndexExists(t, ctx, b, name) {
			built++
		}
	}
	for name := range dropped0049 {
		if validIndexExists(t, ctx, b, name) {
			dropped++
		}
	}
	return built, dropped
}

// validIndexExists reports whether the index exists and the planner may use it: an interrupted concurrent build or
// drop leaves one that is not, yet still maintained on every write.
func validIndexExists(t *testing.T, ctx context.Context, b *PostgresStorageBackend, name string) bool {
	t.Helper()
	var ok bool
	if err := b.db.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM pg_index i JOIN pg_class c ON c.oid = i.indexrelid
		WHERE c.relname = $1 AND c.relnamespace = current_schema()::regnamespace AND i.indisvalid)`, name).Scan(&ok); err != nil {
		t.Fatal(err)
	}
	return ok
}

// interruptDrop leaves name as an interrupted DROP INDEX CONCURRENTLY does: it marks the index invalid and commits,
// then waits for the transactions on its table (an open reader here); cancelled there, the index stays, invalid but
// still maintained.
func interruptDrop(t *testing.T, b *PostgresStorageBackend, name, table string) {
	t.Helper()
	ctx := context.Background()
	reader := openSession(t, b)
	reader.must([]string{`SELECT count(*) FROM ` + table})
	conn, err := b.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var pid int
	if err := conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { _, err := conn.ExecContext(ctx, `DROP INDEX CONCURRENTLY IF EXISTS `+name); done <- err }()
	waitForTheStep(t, b, &step48{pid: pid, done: done})
	// Cancelled by the server, as a shutdown's or a lost lease's cancel is: a client that only hangs up leaves the
	// statement running until it next reads from the client.
	if _, err := b.db.ExecContext(ctx, `SELECT pg_cancel_backend($1)`, pid); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err == nil {
		t.Fatal("the drop finished; the fixture leaves no invalid index")
	}
	_ = reader.tx.Rollback()
	if !indexExists(t, b, name) || validIndexExists(t, ctx, b, name) {
		t.Fatalf("after the interrupted drop %s should exist, invalid", name)
	}
}

// TestMigration0049_UpDownUp: at v49 the step's two indexes exist; once SchemaUpkeep has run the users indexes do too
// and the retired ones are gone; a rollback restores the v48 set valid (the never-built HNSW index stays absent), also
// one the upkeep's drop left invalid when it was interrupted (UPK-2); a re-apply and its upkeep restore the v49 shape.
func TestMigration0049_UpDownUp(t *testing.T) {
	b, dsn := openIsolatedMigrationDB(t)
	ctx := context.Background()
	retired := len(dropped0049) - 1 // the HNSW index never existed
	if built, dropped := v49Indexes(t, ctx, b); built != len(stepIndexes0049) || dropped != retired {
		t.Fatalf("at v49 after boot: %d indexes built, %d retired ones left; want %d, %d (the rest is the upkeep's)", built, dropped, len(stepIndexes0049), retired)
	}
	upkept(t, b)
	if built, dropped := v49Indexes(t, ctx, b); built != len(indexes0049) || dropped != 0 {
		t.Fatalf("at v49 after the upkeep: %d of %d indexes built, %d retired ones left; want all, none", built, len(indexes0049), dropped)
	}
	// A later boot's upkeep was interrupted while it dropped the audit index again (a release that had brought it back).
	if _, err := b.db.ExecContext(ctx, dropped0049["audit_events_org_created_idx"]); err != nil {
		t.Fatal(err)
	}
	interruptDrop(t, b, "audit_events_org_created_idx", "audit_events")
	rolled, err := b.MigrateDown(ctx, 48)
	if err != nil || len(rolled) == 0 || rolled[len(rolled)-1] != 49 {
		t.Fatalf("MigrateDown(48) = %v, %v; want a list ending in 49", rolled, err)
	}
	if built, dropped := v49Indexes(t, ctx, b); built != 0 || dropped != retired {
		t.Fatalf("after the rollback: %d indexes built, %d retired ones back; want 0, %d", built, dropped, retired)
	}
	b2, err := New(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("reconnect (re-apply v49): %v", err)
	}
	t.Cleanup(func() { b2.Close() })
	upkept(t, b2)
	if built, dropped := v49Indexes(t, ctx, b2); built != len(indexes0049) || dropped != 0 {
		t.Fatalf("after the re-apply and its upkeep: %d indexes built, %d retired ones left", built, dropped)
	}
}

// leadingColumn is the first column of the named index.
func leadingColumn(t *testing.T, b *PostgresStorageBackend, index string) string {
	t.Helper()
	var col string
	if err := b.db.QueryRowContext(context.Background(), `SELECT a.attname FROM pg_index i
		JOIN pg_attribute a ON a.attrelid = i.indrelid AND a.attnum = i.indkey[0]
		WHERE i.indexrelid = to_regclass($1)`, index).Scan(&col); err != nil {
		t.Fatalf("leading column of %s: %v", index, err)
	}
	return col
}

// TestMigration0049_ThePreviousReleasesReadsStayKeyed: the release before reads the tables whose indexes v49 retires
// with the statements below; on v49, once the upkeep has dropped them, each still reaches its table through an index
// whose leading column the statement's condition is on (the replacement, or the unique constraint whose leading column
// the dropped index duplicated), never one it can only walk end to end. A retired index costs that release speed at
// most, never a result: none is unique or named by an ON CONFLICT.
func TestMigration0049_ThePreviousReleasesReadsStayKeyed(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	upkept(t, b)
	for _, s := range []string{
		`INSERT INTO organisations (id, name, owner_id) SELECT 'm49-org-' || g, 'Org', '' FROM generate_series(1, 200) g`,
		`INSERT INTO knowledge_documents (id, org_id, filename)
			SELECT 'm49-kd-' || g || '-' || k, 'm49-org-' || g, 'f' || k || '.md' FROM generate_series(1, 200) g, generate_series(1, 5) k`,
		`INSERT INTO org_custom_rules (id, org_id, rule_id, config)
			SELECT 'm49-cr-' || g || '-' || k, 'm49-org-' || g, 'r' || k, '{}' FROM generate_series(1, 200) g, generate_series(1, 5) k`,
		`ANALYZE organisations, knowledge_documents, org_custom_rules`,
	} {
		if _, err := b.db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
	}
	// Either org_id-led index of knowledge_documents serves a lookup by organisation; (id, org_id) does not.
	byOrg := []string{"knowledge_documents_org_created_idx", "knowledge_documents_org_filename_idx"}
	for _, tc := range []struct {
		what, q string
		indexes []string
	}{
		// v48's ListKnowledgeDocuments page (ordered by created_at only) and the re-upload's document count.
		{"knowledge document list", `SELECT id, org_id, filename, created_at, chunker_version FROM knowledge_documents
			WHERE org_id = $1 ORDER BY created_at DESC LIMIT 50 OFFSET 0`, []string{"knowledge_documents_org_created_idx"}},
		{"knowledge document count", `SELECT COUNT(*)::int FROM knowledge_documents WHERE org_id = $1`, byOrg},
		{"organisation delete", `DELETE FROM knowledge_documents WHERE org_id = $1`, byOrg},
		// ListOrgCustomRules and the organisation delete.
		{"custom rule list", `SELECT id, rule_id, config, enabled FROM org_custom_rules WHERE org_id = $1`, []string{"org_custom_rules_org_id_rule_id_key"}},
		{"custom rule delete", `DELETE FROM org_custom_rules WHERE org_id = $1`, []string{"org_custom_rules_org_id_rule_id_key"}},
	} {
		nodes := scans(explainAsUser(t, b, "", tc.q, []any{"m49-org-7"}))
		keyed := false
		for _, n := range nodes {
			if n.NodeType == "Seq Scan" {
				t.Errorf("%s: sequential scan on %s:\n%s", tc.what, n.RelationName, describePlan(nodes))
			}
			if slices.Contains(tc.indexes, n.IndexName) && strings.HasPrefix(n.IndexCond, "("+leadingColumn(t, b, n.IndexName)+" = ") {
				keyed = true
			}
		}
		if !keyed {
			t.Errorf("%s: no probe of %v on its leading column:\n%s", tc.what, tc.indexes, describePlan(nodes))
		}
	}
}

// TestMigration0049_UploadsNeverWaitForTheScanner: the step builds on rule_notices, which the scanner writes, and on
// knowledge_documents, which every upload writes, and holds each SHARE lock to its commit. While a scanner page is in
// flight it must not hold knowledge_documents, so an upload waits for nothing; built in the old order, every upload
// waited for the scanner's page (the review R4, SEA-7).
func TestMigration0049_UploadsNeverWaitForTheScanner(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	if rolled, err := b.MigrateDown(ctx, 48); err != nil || len(rolled) == 0 || rolled[len(rolled)-1] != 49 {
		t.Fatalf("MigrateDown(48) = %v, %v", rolled, err)
	}
	for _, s := range []string{
		`INSERT INTO organisations (id, name, owner_id) VALUES ('s49-org', 'Org', '')`,
	} {
		if _, err := b.db.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	scanner := openSession(t, b)
	scanner.must([]string{`INSERT INTO rule_notices (id, kind, scope, scope_id, rule_set_digest) VALUES ('s49-n', 'rules_changed', 'org', 's49-org', 'd')`})

	conn, err := b.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var pid int
	if err := conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- applyMigrationOnce(ctx, conn, mustMigrationByVersion(49)) }()
	waitForTheStep(t, b, &step48{pid: pid, done: done})

	start := time.Now()
	uctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	_, err = b.db.ExecContext(uctx, `INSERT INTO knowledge_documents (id, org_id, filename) VALUES ('s49-kd', 's49-org', 'a.md')`)
	cancel()
	if took := time.Since(start); err != nil || took > 500*time.Millisecond {
		t.Errorf("an upload while the step waits for the scanner's page: %v after %v; want it written at once", err, took.Round(time.Millisecond))
	}
	time.Sleep(300 * time.Millisecond)
	if err := scanner.tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the step: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the step did not end")
	}
}

// TestMigration0049_NoDeadlockWithAnOrganisationDelete: the release before deletes an organisation's knowledge
// documents first and its row last, whose cascade reaches rule_notices. A step that held rule_notices while it waited
// for knowledge_documents deadlocked with it (measured under load: the step lost, one retry); one that held
// knowledge_documents while it waited for the scanner's page on rule_notices made every upload wait. It takes the
// two locks all or none.
func TestMigration0049_NoDeadlockWithAnOrganisationDelete(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	if rolled, err := b.MigrateDown(ctx, 48); err != nil || len(rolled) == 0 || rolled[len(rolled)-1] != 49 {
		t.Fatalf("MigrateDown(48) = %v, %v", rolled, err)
	}
	for _, s := range []string{
		`INSERT INTO organisations (id, name, owner_id) VALUES ('d49-org', 'Org', '')`,
		`INSERT INTO knowledge_documents (id, org_id, filename) VALUES ('d49-kd', 'd49-org', 'a.md')`,
		`INSERT INTO rule_notices (id, kind, scope, scope_id, rule_set_digest) VALUES ('d49-n', 'rules_changed', 'org', 'd49-org', 'd')`,
	} {
		if _, err := b.db.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	// The organisation delete in flight: its first step done.
	req := openSession(t, b)
	req.must([]string{`DELETE FROM knowledge_documents WHERE org_id = 'd49-org'`})

	conn, err := b.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var pid int
	if err := conn.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- applyMigrationOnce(ctx, conn, mustMigrationByVersion(49)) }()
	waitForTheStep(t, b, &step48{pid: pid, done: done})
	if err := req.run([]string{`DELETE FROM policies WHERE org_id = 'd49-org'`, `DELETE FROM organisations WHERE id = 'd49-org'`}); err != nil {
		t.Errorf("the organisation delete: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("the step, run once: %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the step did not end")
	}
}
