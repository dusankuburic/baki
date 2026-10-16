package database

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/lifecycle"
	"pad-analyzer/internal/platform/lock"
)

// TestSchemaUpkeep_ValidatesAndBuildsOnce: after boot v48's foreign keys are not yet validated and the actor indexes
// not built; SchemaUpkeep, under its lease, validates the keys and builds the indexes, records a successful run, and a
// second run finds nothing to do.
func TestSchemaUpkeep_ValidatesAndBuildsOnce(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	if s := v48Shape(t, ctx, b); s.validated != 0 || s.upkeepIndexes != 0 {
		t.Fatalf("after boot: %+v; want no validated key and no actor index yet (the upkeep has not run)", s)
	}
	if err := b.SchemaUpkeep(ctx); err != nil {
		t.Fatalf("SchemaUpkeep: %v", err)
	}
	if s := v48Shape(t, ctx, b); s.validated != len(v48Constraints) || s.upkeepIndexes != len(v48UpkeepIndexes) {
		t.Fatalf("after the upkeep: %+v; want every key validated and every actor index built", s)
	}
	if at, err := b.JobLastSuccess(ctx, schemaUpkeepJob.Name()); err != nil || at.IsZero() {
		t.Errorf("the job's last success = %v, %v; want the run recorded", at, err)
	}
	if err := b.SchemaUpkeep(ctx); err != nil {
		t.Fatalf("SchemaUpkeep again: %v", err)
	}
}

// TestSchemaUpkeep_WaitsWhileAnotherReplicaRunsIt: the lease is held elsewhere, so this replica's upkeep changes
// nothing and waits for that run to end before it returns (its caller's HNSW build of knowledge_chunks would otherwise
// run against the other replica's VALIDATE of that table's key); then it runs, and finds nothing left to do, or what
// the other left.
func TestSchemaUpkeep_WaitsWhileAnotherReplicaRunsIt(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	defer func(was time.Duration) { upkeepHeldPoll = was }(upkeepHeldPoll)
	upkeepHeldPoll = 50 * time.Millisecond
	conn, err := b.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var got bool
	if err := conn.QueryRowContext(ctx, `SELECT pg_try_advisory_lock($1)`, schemaUpkeepJob.Key()).Scan(&got); err != nil || !got {
		t.Fatalf("take the job's lock: %v, %v", got, err)
	}
	done := make(chan error, 1)
	go func() { done <- b.SchemaUpkeep(ctx) }()
	select {
	case err := <-done:
		t.Fatalf("SchemaUpkeep returned (%v) while another holder runs it; want it to wait", err)
	case <-time.After(500 * time.Millisecond):
	}
	if s := v48Shape(t, ctx, b); s.validated != 0 || s.upkeepIndexes != 0 {
		t.Errorf("the waiting upkeep changed the schema: %+v", s)
	}
	if _, err := conn.ExecContext(ctx, `SELECT pg_advisory_unlock($1)`, schemaUpkeepJob.Key()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("SchemaUpkeep after the other run ended: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("SchemaUpkeep did not end once the lease was free")
	}
	if s := v48Shape(t, ctx, b); s.validated != len(v48Constraints) || s.upkeepIndexes != len(v48UpkeepIndexes) {
		t.Errorf("after the wait: %+v; want the run done", s)
	}
}

// TestSchemaUpkeep_ShutdownStopsItAndItsStatement: shutdown stops the job (lifecycle's drain) while a concurrent build
// waits. SchemaUpkeep then says so (ErrJobStopped), so its caller starts no HNSW build during the shutdown (UPK-1),
// and the build is cancelled on the server too: the driver only drops the connection, and the statement would go on
// without it, holding its table, until it next wrote to the client.
func TestSchemaUpkeep_ShutdownStopsItAndItsStatement(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	if _, err := b.db.ExecContext(ctx, `INSERT INTO users (id, email, password, role) VALUES ('d49-u', 'd49-u@example.com', 'x', 'member')`); err != nil {
		t.Fatal(err)
	}
	// A write in flight on flow_versions: the concurrent build there waits for it.
	writer := openSession(t, b)
	writer.must([]string{`INSERT INTO flows (id, name, owner_id) VALUES ('d49-flow', 'F', 'd49-u')`,
		`INSERT INTO flow_versions (id, flow_id, version, created_by) VALUES ('d49-v', 'd49-flow', 1, 'd49-u')`})
	g := &lifecycle.Group{}
	done := make(chan error, 1)
	go func() { done <- b.schemaUpkeep(ctx, g.NewSingleton) }()
	building := func() (n int) {
		t.Helper()
		if err := b.db.QueryRowContext(ctx, `SELECT count(*) FROM pg_stat_activity
			WHERE datname = current_database() AND query LIKE 'CREATE INDEX CONCURRENTLY%flow_versions_created_by_idx%' AND state = 'active'`).Scan(&n); err != nil {
			t.Fatal(err)
		}
		return n
	}
	deadline := time.Now().Add(10 * time.Second)
	for building() == 0 {
		if time.Now().After(deadline) {
			t.Fatal("the upkeep never reached the flow_versions build")
		}
		time.Sleep(10 * time.Millisecond)
	}
	dctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := g.Drain(dctx); err != nil {
		t.Fatalf("drain: %v", err)
	}
	select {
	case err := <-done:
		if !errors.Is(err, lifecycle.ErrJobStopped) {
			t.Errorf("SchemaUpkeep after the drain = %v; want lifecycle.ErrJobStopped, so its caller starts nothing more", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("SchemaUpkeep did not return after the drain")
	}
	deadline = time.Now().Add(3 * time.Second)
	for building() > 0 {
		if time.Now().After(deadline) {
			t.Fatal("the build goes on in the server after the upkeep was stopped")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestSchemaUpkeep_LeavesANewerSchemaAlone: on a schema a later release moved ahead (expand-only, so this binary
// boots), that release's own upkeep owns the indexes: this one changes nothing, so a retired index name the later
// release built again is never dropped by a replica of this one booting during its deploy or after a rollback (UPK-3).
func TestSchemaUpkeep_LeavesANewerSchemaAlone(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	upkept(t, b)
	latest := migrations[len(migrations)-1].version
	for _, s := range []string{
		dropped0049["audit_events_org_created_idx"],
		fmt.Sprintf(`INSERT INTO schema_migrations (version, name, checksum, compat) VALUES (%d, 'org_audit_view', 'x', 'expand')`, latest+1),
	} {
		if _, err := b.db.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	upkept(t, b)
	if !validIndexExists(t, ctx, b, "audit_events_org_created_idx") {
		t.Error("the upkeep of an older binary dropped an index the newer schema built again")
	}
}

// TestSchemaUpkeep_SkipsAPoolTooSmallForIt: the upkeep holds two connections (its lease and its work) for its whole
// run; on a pool of one the lease took the only one, and the work and every request of the replica waited for one
// until the 2 h deadline (UPK-4). It skips instead, and requests go on.
func TestSchemaUpkeep_SkipsAPoolTooSmallForIt(t *testing.T) {
	_, dsn := openIsolatedMigrationDB(t)
	cfg := DefaultConfig(dsn)
	cfg.MaxOpenConns = 1
	b, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	if err := b.SchemaUpkeep(ctx); err != nil || time.Since(start) > time.Second {
		t.Errorf("SchemaUpkeep on a pool of one = %v after %v; want a skip at once", err, time.Since(start).Round(time.Millisecond))
	}
	if err := b.db.PingContext(ctx); err != nil {
		t.Errorf("a request after it: %v", err)
	}
	if s := v48Shape(t, context.Background(), b); s.validated != 0 || s.upkeepIndexes != 0 {
		t.Errorf("the skipped upkeep changed the schema: %+v", s)
	}
}

// TestSchemaUpkeep_CleansUpWhatACheckRefuses: a CHECK a later step adds NOT VALID (the README's pending policy id
// CHECK) gets the same cleanup as a foreign key: a row it refuses is fixed by the item's statement, then validated
// (UPK-5).
func TestSchemaUpkeep_CleansUpWhatACheckRefuses(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	for _, s := range []string{
		`INSERT INTO organisations (id, name, owner_id) VALUES ('k50-org', 'Org', '')`,
		`INSERT INTO policies (id, org_id, name) VALUES ('', 'k50-org', 'Id-less')`,
		`ALTER TABLE policies ADD CONSTRAINT policies_id_not_empty CHECK (id <> '') NOT VALID`,
	} {
		if _, err := b.db.ExecContext(ctx, s); err != nil {
			t.Fatalf("%s: %v", s, err)
		}
	}
	conn, err := b.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	c := pendingConstraint{50, "policies", "policies_id_not_empty", `UPDATE policies SET id = gen_random_uuid()::text WHERE id = ''`}
	if err := validateConstraint(ctx, conn, c); err != nil {
		t.Fatalf("validate the CHECK: %v", err)
	}
	var valid bool
	if err := b.db.QueryRowContext(ctx, `SELECT convalidated FROM pg_constraint WHERE conname = 'policies_id_not_empty'`).Scan(&valid); err != nil || !valid {
		t.Errorf("the CHECK validated = %v, %v", valid, err)
	}
}

// TestSchemaUpkeep_DefersAKeyWhoseTableIsBeingIndexed: another replica's HNSW build of knowledge_chunks holds the
// table's SHARE UPDATE EXCLUSIVE lock for minutes; the VALIDATE of its key would wait 5 s three times and fail anyway.
// It is left for the next boot at once.
func TestSchemaUpkeep_DefersAKeyWhoseTableIsBeingIndexed(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	for _, s := range []string{
		`INSERT INTO organisations (id, name, owner_id) VALUES ('h49-org', 'Org', '')`,
		`INSERT INTO knowledge_documents (id, org_id, filename) VALUES ('h49-kd', 'h49-org', 'a.md')`,
	} {
		if _, err := b.db.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	// The other build waits for a write in flight, as a long build is busy, holding the table.
	writer := openSession(t, b)
	writer.must([]string{`INSERT INTO knowledge_chunks (id, doc_id, org_id, content, embedding) VALUES ('h49-kc', 'h49-kd', 'h49-org', 'x', '[]')`})
	other, err := b.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var pid int
	if err := other.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	built := make(chan error, 1)
	go func() {
		_, err := other.ExecContext(ctx, `CREATE INDEX CONCURRENTLY h49_kc_idx ON knowledge_chunks (doc_id)`)
		built <- err
	}()
	waitForTheStep(t, b, &step48{pid: pid, done: built})
	conn, err := b.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	var c pendingConstraint
	for _, pc := range schemaUpkeepConstraints {
		if pc.table == "knowledge_chunks" {
			c = pc
		}
	}
	start := time.Now()
	err = upkeepRetry(ctx, func() error { return validateConstraint(ctx, conn, c) })
	if took := time.Since(start); err == nil || took > 2*time.Second {
		t.Errorf("the key of a table being indexed = %v after %v; want it deferred at once", err, took.Round(time.Millisecond))
	}
	if err := writer.tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-built; err != nil {
		t.Fatalf("the other build: %v", err)
	}
	if err := validateConstraint(ctx, conn, c); err != nil {
		t.Errorf("once the build ended: %v", err)
	}
}

// TestSchemaUpkeep_ValidatesKeyWhileSiblingDatabaseBuildsIndex: pg_stat_progress_create_index is cluster-wide and
// databases cloned from the same template share table OIDs (conrelid). validateConstraint filters by current_database()
// (p.datid), so an index build in a sibling database does not defer constraint validation in this database.
func TestSchemaUpkeep_ValidatesKeyWhileSiblingDatabaseBuildsIndex(t *testing.T) {
	b1, _ := openIsolatedMigrationDB(t)
	b2, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	for _, s := range []string{
		`INSERT INTO organisations (id, name, owner_id) VALUES ('sib-org', 'Org', '')`,
		`INSERT INTO knowledge_documents (id, org_id, filename) VALUES ('sib-kd', 'sib-org', 'a.md')`,
	} {
		if _, err := b2.db.ExecContext(ctx, s); err != nil {
			t.Fatal(err)
		}
	}
	writer := openSession(t, b2)
	writer.must([]string{`INSERT INTO knowledge_chunks (id, doc_id, org_id, content, embedding) VALUES ('sib-kc', 'sib-kd', 'sib-org', 'x', '[]')`})
	other, err := b2.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	var pid int
	if err := other.QueryRowContext(ctx, `SELECT pg_backend_pid()`).Scan(&pid); err != nil {
		t.Fatal(err)
	}
	built := make(chan error, 1)
	go func() {
		_, err := other.ExecContext(ctx, `CREATE INDEX CONCURRENTLY sib_kc_idx ON knowledge_chunks (doc_id)`)
		built <- err
	}()
	waitForTheStep(t, b2, &step48{pid: pid, done: built})

	conn1, err := b1.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn1.Close()
	var c pendingConstraint
	for _, pc := range schemaUpkeepConstraints {
		if pc.table == "knowledge_chunks" {
			c = pc
		}
	}
	if err := validateConstraint(ctx, conn1, c); err != nil {
		t.Errorf("validateConstraint in b1 while b2 builds index: %v, want nil", err)
	}
	var valid bool
	if err := b1.db.QueryRowContext(ctx, `SELECT convalidated FROM pg_constraint WHERE conname = $1`, c.name).Scan(&valid); err != nil || !valid {
		t.Errorf("constraint %s convalidated in b1 = %v, %v; want true", c.name, valid, err)
	}

	if err := writer.tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := <-built; err != nil {
		t.Fatalf("sibling build: %v", err)
	}
}

// TestSchemaUpkeep_RebuildsAnInvalidIndex: a concurrent build that failed leaves an invalid index of the name, which
// IF NOT EXISTS would keep forever (unused, yet maintained on every write). The upkeep drops it and builds the index.
func TestSchemaUpkeep_RebuildsAnInvalidIndex(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	for _, s := range []string{
		`INSERT INTO users (id, email, password, role) VALUES ('u49-u', 'u49-u@example.com', 'x', 'member')`,
		`INSERT INTO flows (id, name, owner_id) VALUES ('u49-flow', 'F', 'u49-u')`,
		`INSERT INTO flow_versions (id, flow_id, version, created_by) VALUES ('u49-v1', 'u49-flow', 1, 'u49-u'), ('u49-v2', 'u49-flow', 2, 'u49-u')`,
	} {
		if _, err := b.db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed: %v", err)
		}
	}
	// A unique build over two equal values fails after creating the index: it stays, invalid.
	if _, err := b.db.ExecContext(ctx, `CREATE UNIQUE INDEX CONCURRENTLY flow_versions_created_by_idx ON flow_versions (created_by)`); err == nil {
		t.Fatal("the unique build over duplicates succeeded; the fixture leaves no invalid index")
	}
	var valid bool
	if err := b.db.QueryRowContext(ctx, `SELECT indisvalid FROM pg_index WHERE indexrelid = 'flow_versions_created_by_idx'::regclass`).Scan(&valid); err != nil || valid {
		t.Fatalf("the leftover index: valid %v, %v; want an invalid one", valid, err)
	}
	upkept(t, b)
	var def string
	if err := b.db.QueryRowContext(ctx, `SELECT i.indisvalid, pg_get_indexdef(i.indexrelid) FROM pg_index i
		WHERE i.indexrelid = 'flow_versions_created_by_idx'::regclass`).Scan(&valid, &def); err != nil {
		t.Fatal(err)
	}
	if !valid || strings.Contains(def, "UNIQUE") || !strings.Contains(def, "WHERE (created_by <> ''::text)") {
		t.Errorf("after the upkeep: valid %v, %s; want the partial, non-unique index rebuilt", valid, def)
	}
}

// TestSchemaUpkeep_DeletesWhatAKeyRefuses: a row written while 0048 ran, after its orphan delete and before its ADD
// CONSTRAINT (a session of an account being erased, a chunk filed under another organisation), would fail the
// VALIDATE at every boot. The upkeep deletes such rows, as the step did, and validates.
func TestSchemaUpkeep_DeletesWhatAKeyRefuses(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	for _, s := range []string{
		`INSERT INTO users (id, email, password, role) VALUES ('k49-u', 'k49-u@example.com', 'x', 'member')`,
		`INSERT INTO organisations (id, name, owner_id) VALUES ('k49-org', 'Org', 'k49-u'), ('k49-org2', 'Org 2', 'k49-u')`,
		`INSERT INTO knowledge_documents (id, org_id, filename) VALUES ('k49-kd', 'k49-org', 'a.md')`,
		`INSERT INTO knowledge_chunks (id, doc_id, org_id, content, embedding) VALUES ('k49-kc', 'k49-kd', 'k49-org', 'x', '[]')`,
		`INSERT INTO refresh_tokens (jti, user_id, expires_at) VALUES ('k49-rt', 'k49-u', NOW() + INTERVAL '1 hour')`,
		// The window: the keys are not there yet.
		`ALTER TABLE refresh_tokens DROP CONSTRAINT refresh_tokens_user_id_fkey`,
		`ALTER TABLE knowledge_chunks DROP CONSTRAINT knowledge_chunks_doc_id_org_id_fkey`,
		`INSERT INTO refresh_tokens (jti, user_id, expires_at) VALUES ('k49-rt-gone', 'k49-erased', NOW() + INTERVAL '1 hour')`,
		`INSERT INTO knowledge_chunks (id, doc_id, org_id, content, embedding) VALUES ('k49-kc-elsewhere', 'k49-kd', 'k49-org2', 'x', '[]')`,
		`ALTER TABLE refresh_tokens ADD CONSTRAINT refresh_tokens_user_id_fkey FOREIGN KEY (user_id) REFERENCES users (id) ON DELETE CASCADE NOT VALID`,
		`ALTER TABLE knowledge_chunks ADD CONSTRAINT knowledge_chunks_doc_id_org_id_fkey FOREIGN KEY (doc_id, org_id) REFERENCES knowledge_documents (id, org_id) ON DELETE CASCADE NOT VALID`,
	} {
		if _, err := b.db.ExecContext(ctx, s); err != nil {
			t.Fatalf("seed: %v\n%s", err, s)
		}
	}
	upkept(t, b)
	if s := v48Shape(t, ctx, b); s.validated != len(v48Constraints) {
		t.Errorf("after the upkeep: %+v; want every key validated", s)
	}
	var left, kept int
	if err := b.db.QueryRowContext(ctx, `SELECT
		(SELECT count(*) FROM refresh_tokens WHERE jti = 'k49-rt-gone') + (SELECT count(*) FROM knowledge_chunks WHERE id = 'k49-kc-elsewhere'),
		(SELECT count(*) FROM refresh_tokens WHERE jti = 'k49-rt') + (SELECT count(*) FROM knowledge_chunks WHERE id = 'k49-kc')`).Scan(&left, &kept); err != nil {
		t.Fatal(err)
	}
	if left != 0 || kept != 2 {
		t.Errorf("%d refused row(s) left and %d of the 2 good ones kept; want 0 and 2", left, kept)
	}
}

// TestSchemaUpkeep_LetsWritesThrough: a VALIDATE takes SHARE UPDATE EXCLUSIVE on its table, so a session and a chunk
// are written while it runs; the step's own locks are the ones TestMigration0048_BlocksWritesOnlyInItsTail checks.
func TestSchemaUpkeep_LetsWritesThrough(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	if _, err := b.db.ExecContext(ctx, `INSERT INTO users (id, email, password, role) VALUES ('w49-u', 'w49-u@example.com', 'x', 'member')`); err != nil {
		t.Fatal(err)
	}
	conn, err := b.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	// The upkeep's statement, held open in its transaction as on a large table.
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, c := range schemaUpkeepConstraints {
		if c.table == "refresh_tokens" {
			if _, err := tx.ExecContext(ctx, `ALTER TABLE `+c.table+` VALIDATE CONSTRAINT `+c.name); err != nil {
				t.Fatal(err)
			}
		}
	}
	wctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	w, err := b.db.Conn(wctx)
	if err != nil {
		t.Fatal(err)
	}
	defer w.Close()
	if _, err := w.ExecContext(wctx, `SET lock_timeout = '2s'`); err != nil {
		t.Fatal(err)
	}
	if _, err := w.ExecContext(wctx, `INSERT INTO refresh_tokens (jti, user_id, expires_at) VALUES ('w49-rt', 'w49-u', NOW() + INTERVAL '1 hour')`); err != nil {
		t.Errorf("a sign-in while the session key is validated: %v; want it written", err)
	}
	if _, err := w.ExecContext(wctx, `UPDATE users SET display_name = 'W' WHERE id = 'w49-u'`); err != nil {
		t.Errorf("an account write while the session key is validated: %v; want it written", err)
	}
	_, _ = w.ExecContext(context.Background(), `RESET lock_timeout`)
}

// TestEnsureVectorIndex_SkipsAPoolTooSmallForIt: the HNSW build holds two connections too (its lease and its build),
// so on a pool of one it skipped nothing and waited for a connection until its deadline, every request with it.
func TestEnsureVectorIndex_SkipsAPoolTooSmallForIt(t *testing.T) {
	_, dsn := openIsolatedMigrationDB(t)
	cfg := DefaultConfig(dsn)
	cfg.MaxOpenConns = 1
	b, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close()
	if !b.hasPgvector {
		t.Skip("pgvector not installed")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	start := time.Now()
	if err := b.EnsureVectorIndex(ctx, 8); err != nil || time.Since(start) > time.Second {
		t.Errorf("EnsureVectorIndex on a pool of one = %v after %v; want a skip at once", err, time.Since(start).Round(time.Millisecond))
	}
	if err := b.db.PingContext(ctx); err != nil {
		t.Errorf("a request after it: %v", err)
	}
}

// busyLeaser is a pool with no connection to spare for the lease: its first fails TryLease calls end as the lease's
// connection wait does when the pool's queue outlasts its budget; then it leases as the backend does.
type busyLeaser struct {
	lock.Leaser
	fails, calls int
	asked        []time.Time // when each TryLease came
}

func (l *busyLeaser) TryLease(ctx context.Context, key int64) (lock.Lease, bool, error) {
	l.calls++
	l.asked = append(l.asked, time.Now())
	if l.calls <= l.fails {
		return nil, false, context.DeadlineExceeded
	}
	return l.Leaser.TryLease(ctx, key)
}

// TestSchemaUpkeep_AsksAgainAtGrowingIntervalsWhileTheLeaseStaysUnavailable: every attempt the lease fails is a WARN
// and a skip of the job's (lifecycle.LogJobLockUnavailable), so a database unreachable for long after boot would log
// one every upkeepHeldPoll for the whole budget (720 for the upkeep, 1440 for the HNSW build, which shares the loop).
// The pause doubles after each failed ask, up to upkeepHeldPollMax.
func TestSchemaUpkeep_AsksAgainAtGrowingIntervalsWhileTheLeaseStaysUnavailable(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	defer func(poll, most time.Duration) { upkeepHeldPoll, upkeepHeldPollMax = poll, most }(upkeepHeldPoll, upkeepHeldPollMax)
	upkeepHeldPoll, upkeepHeldPollMax = 20*time.Millisecond, 80*time.Millisecond
	busy := &busyLeaser{Leaser: b, fails: 8}
	newJob := func(j lifecycle.Job, _ lock.Leaser, s lifecycle.JobState) *lifecycle.Singleton {
		return lifecycle.NewSingleton(j, busy, s)
	}
	if err := b.schemaUpkeep(ctx, newJob); err != nil {
		t.Fatalf("SchemaUpkeep behind a lease that fails eight times: %v", err)
	}
	if busy.calls != busy.fails+1 {
		t.Fatalf("the upkeep asked for its lease %d time(s); want %d (once after each failure, then once more)", busy.calls, busy.fails+1)
	}
	// The eight pauses: 20, 40, 80 ms, then 80 ms five times = 540 ms. Every 20 ms would be 160 ms; doubling without
	// the cap 5.1 s. A sleep never ends early, and a loaded machine stretches 540 ms nowhere near 2.5 s.
	waited := busy.asked[len(busy.asked)-1].Sub(busy.asked[0])
	if waited < 530*time.Millisecond || waited > 2500*time.Millisecond {
		t.Errorf("the eight pauses between asks took %v in all; want about 540 ms (20, 40, then 80 ms each)", waited.Round(time.Millisecond))
	}
	if s := v48Shape(t, ctx, b); s.validated != len(v48Constraints) {
		t.Errorf("after the upkeep: %+v; want the run done once the lease was there", s)
	}
}

// TestSchemaUpkeep_WaitsForAConnectionWhenThePoolIsBusy: in its first minutes a replica serves a full pool, and the
// lease's connection wait can outlast its budget. That ended the upkeep for the boot before it had done anything
// (measured under load: six keys left NOT VALID, and the HNSW build skipped the same way). It asks again, as it does
// while another replica holds the lease.
func TestSchemaUpkeep_WaitsForAConnectionWhenThePoolIsBusy(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	defer func(was time.Duration) { upkeepHeldPoll = was }(upkeepHeldPoll)
	upkeepHeldPoll = 20 * time.Millisecond
	busy := &busyLeaser{Leaser: b, fails: 3}
	newJob := func(j lifecycle.Job, _ lock.Leaser, s lifecycle.JobState) *lifecycle.Singleton {
		return lifecycle.NewSingleton(j, busy, s)
	}
	if err := b.schemaUpkeep(ctx, newJob); err != nil {
		t.Fatalf("SchemaUpkeep behind a busy pool: %v", err)
	}
	if busy.calls <= busy.fails {
		t.Errorf("the upkeep asked for its lease %d time(s); want it to ask again after the pool had no connection to spare", busy.calls)
	}
	if s := v48Shape(t, ctx, b); s.validated != len(v48Constraints) || s.upkeepIndexes != len(v48UpkeepIndexes) {
		t.Errorf("after the upkeep: %+v; want the run done once the pool had a connection", s)
	}
}

// TestEnsureVectorIndex_WaitsForAConnectionWhenThePoolIsBusy: the HNSW build the same, after the upkeep's own wait.
func TestEnsureVectorIndex_WaitsForAConnectionWhenThePoolIsBusy(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	if !b.hasPgvector {
		t.Skip("pgvector not installed")
	}
	ctx := context.Background()
	defer func(was time.Duration) { upkeepHeldPoll = was }(upkeepHeldPoll)
	upkeepHeldPoll = 20 * time.Millisecond
	busy := &busyLeaser{Leaser: b, fails: 3}
	newJob := func(j lifecycle.Job, _ lock.Leaser, s lifecycle.JobState) *lifecycle.Singleton {
		return lifecycle.NewSingleton(j, busy, s)
	}
	if err := b.ensureVectorIndex(ctx, 8, newJob); err != nil {
		t.Fatalf("EnsureVectorIndex behind a busy pool: %v", err)
	}
	if busy.calls <= busy.fails {
		t.Errorf("the build asked for its lease %d time(s); want it to ask again after the pool had no connection to spare", busy.calls)
	}
	if !validIndexExists(t, ctx, b, knowledgeVectorIndexName(8)) {
		t.Errorf("%s was not built once the pool had a connection", knowledgeVectorIndexName(8))
	}
}
