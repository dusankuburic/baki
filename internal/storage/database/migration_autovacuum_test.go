package database

import (
	"context"
	"testing"
	"time"
)

// TestMigration0048_DisplacesAnAutovacuumWorker verifies RV-GAP0-1: an autovacuum worker holding
// ShareUpdateExclusiveLock on refresh_tokens is displaced once the migration DO block reaches
// deadlock_timeout on its lock wait.
func TestMigration0048_DisplacesAnAutovacuumWorker(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	seed48Concurrency(t, b)
	ctx := context.Background()
	conn, err := b.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	restore, err := liftSessionTimeouts(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(cctx, `ALTER TABLE refresh_tokens SET (autovacuum_enabled = false)`)
		restore()
		conn.Close()
	})

	for _, s := range []string{
		`ALTER TABLE refresh_tokens SET (autovacuum_vacuum_cost_delay = 100, autovacuum_vacuum_cost_limit = 1,
			autovacuum_vacuum_threshold = 100, autovacuum_vacuum_scale_factor = 0)`,
		`INSERT INTO refresh_tokens (jti, user_id, expires_at)
			SELECT 'av48-' || g, 'c48-owner', NOW() + INTERVAL '1 hour' FROM generate_series(1, 400000) g`,
		`DELETE FROM refresh_tokens WHERE jti LIKE 'av48-%' AND right(jti, 1) IN ('0', '2', '4', '6', '8')`,
	} {
		if _, err := conn.ExecContext(ctx, s); err != nil {
			t.Fatalf("prepare: %v\n%s", err, s)
		}
	}

	const workerQ = `SELECT l.pid FROM pg_locks l WHERE l.relation = 'refresh_tokens'::regclass
		AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
		AND l.mode = 'ShareUpdateExclusiveLock' AND l.granted AND l.pid <> pg_backend_pid() LIMIT 1`

	var pid int
	waitStart := time.Now()
	for {
		if err := conn.QueryRowContext(ctx, workerQ).Scan(&pid); err == nil {
			break
		}
		if time.Since(waitStart) > 120*time.Second {
			t.Skip("no autovacuum worker on refresh_tokens within 120 s")
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("autovacuum worker pid %d holds ShareUpdateExclusiveLock on refresh_tokens", pid)

	if err := applyMigration(ctx, conn, mustMigrationByVersion(48)); err != nil {
		t.Fatalf("applyMigration(48) failed against autovacuum: %v", err)
	}
}

// TestMigration0049_DisplacesAnAutovacuumWorker verifies that migration 0049 displaces an
// autovacuum worker holding ShareUpdateExclusiveLock on knowledge_documents.
func TestMigration0049_DisplacesAnAutovacuumWorker(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	seed48Concurrency(t, b)
	ctx := context.Background()
	conn, err := b.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	restore, err := liftSessionTimeouts(ctx, conn)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		cctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
		defer cancel()
		_, _ = conn.ExecContext(cctx, `ALTER TABLE knowledge_documents SET (autovacuum_enabled = false)`)
		restore()
		conn.Close()
	}()

	if err := applyMigration(ctx, conn, mustMigrationByVersion(48)); err != nil {
		t.Fatalf("applyMigration(48): %v", err)
	}

	for _, s := range []string{
		`ALTER TABLE knowledge_documents SET (autovacuum_vacuum_cost_delay = 100, autovacuum_vacuum_cost_limit = 1,
			autovacuum_vacuum_threshold = 100, autovacuum_vacuum_scale_factor = 0)`,
		`INSERT INTO knowledge_documents (id, org_id, filename, created_at)
			SELECT 'av49-doc-' || g, 'c48-org', 'file-' || g || '.txt', NOW()
			FROM generate_series(1, 400000) g`,
		`DELETE FROM knowledge_documents WHERE id LIKE 'av49-doc-%' AND right(id, 1) IN ('0', '2', '4', '6', '8')`,
	} {
		if _, err := conn.ExecContext(ctx, s); err != nil {
			t.Fatalf("prepare: %v\n%s", err, s)
		}
	}

	const workerQ = `SELECT l.pid FROM pg_locks l WHERE l.relation = 'knowledge_documents'::regclass
		AND l.database = (SELECT oid FROM pg_database WHERE datname = current_database())
		AND l.mode = 'ShareUpdateExclusiveLock' AND l.granted AND l.pid <> pg_backend_pid() LIMIT 1`

	var pid int
	waitStart := time.Now()
	for {
		if err := conn.QueryRowContext(ctx, workerQ).Scan(&pid); err == nil {
			break
		}
		if time.Since(waitStart) > 120*time.Second {
			t.Skip("no autovacuum worker on knowledge_documents within 120 s")
		}
		time.Sleep(250 * time.Millisecond)
	}
	t.Logf("autovacuum worker pid %d holds ShareUpdateExclusiveLock on knowledge_documents", pid)

	if err := applyMigration(ctx, conn, mustMigrationByVersion(49)); err != nil {
		t.Fatalf("applyMigration(49) failed against autovacuum: %v", err)
	}
}
