package database

import (
	"context"
	"testing"
	"time"
)

// TestMigrateDown_RoundTrip is the DATABASE_URL-gated integration test for the
// down-path: roll back the latest step, assert the recorded version dropped,
// then reconnect (forward migrate re-applies it) and assert we're back at the
// latest with no checksum drift. Proves the down-migration is a faithful inverse
// and the checksum gate survives a down→up cycle.
func TestMigrateDown_RoundTrip(t *testing.T) {
	b, dsn := openIsolatedMigrationDB(t)
	ctx := context.Background()

	latest := migrations[len(migrations)-1].version
	if latest < 2 {
		t.Skip("need at least 2 migrations to roll one back")
	}
	current, err := b.CurrentSchemaVersion(ctx)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	if current != latest {
		t.Skipf("DB not at latest (v%d); another integration test may have rolled it back. Skipping round-trip.", current)
	}

	target := latest - 1

	rolled, err := b.MigrateDown(ctx, target)
	if err != nil {
		t.Fatalf("MigrateDown(v%d): %v", target, err)
	}
	if len(rolled) != 1 || rolled[0] != latest {
		t.Fatalf("rolled back %v, want [%d]", rolled, latest)
	}
	got, err := b.CurrentSchemaVersion(ctx)
	if err != nil {
		t.Fatalf("read version after down: %v", err)
	}
	if got != target {
		t.Fatalf("after down, version = %d, want %d", got, target)
	}

	// Reconnect: boot-time forward migrate() must re-apply the rolled-back step
	// idempotently and end up back at latest with no checksum drift (i.e. New
	// returns without error). This also documents the boot-reapply gotcha: a
	// manual down is undone by the next server boot unless the binary is rolled
	// back too.
	b2, err := New(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("reconnect (forward re-apply) failed: %v — drift after down→up?", err)
	}
	defer b2.Close()
	got2, err := b2.CurrentSchemaVersion(ctx)
	if err != nil {
		t.Fatalf("read version after re-apply: %v", err)
	}
	if got2 != latest {
		t.Errorf("after re-apply, version = %d, want latest %d", got2, latest)
	}
}

// TestMigrateDown_NoOpWhenAlreadyAtOrBelowTarget asserts the driver short-
// circuits cleanly when there's nothing to roll back (no error, no rows touched).
func TestMigrateDown_NoOpWhenAlreadyAtOrBelowTarget(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	current, err := b.CurrentSchemaVersion(ctx)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	rolled, err := b.MigrateDown(ctx, current) // target == current
	if err != nil {
		t.Fatalf("MigrateDown(current): %v", err)
	}
	if len(rolled) != 0 {
		t.Errorf("expected no rollbacks when target==current, got %v", rolled)
	}
	// Target above current is also a no-op (nothing to do).
	rolled2, err := b.MigrateDown(ctx, current+5)
	if err != nil {
		t.Fatalf("MigrateDown(current+5): %v", err)
	}
	if len(rolled2) != 0 {
		t.Errorf("expected no rollbacks when target>current, got %v", rolled2)
	}
}

// TestMigrateDown_WaitsBoundedForEachLock: a down step holds every lock it takes until it commits, and a lock it
// waits for holds up every later query on that table. Behind a reader of users (an operator's session, a replica not
// yet stopped), 0049's down waited for as long as the reader lived, and every sign-in's read of users with it. Like a
// step going up, it now waits at most the step's lock_timeout, then rolls back, pauses and tries again.
func TestMigrateDown_WaitsBoundedForEachLock(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()
	defer func(timeout, backoff time.Duration) { migrationLockTimeout, migrationLockBackoff = timeout, backoff }(migrationLockTimeout, migrationLockBackoff)
	migrationLockTimeout, migrationLockBackoff = 300*time.Millisecond, 200*time.Millisecond
	upkept(t, b) // the users indexes the down drops
	reader := openSession(t, b)
	reader.must([]string{`SELECT count(*) FROM users`})

	done := make(chan error, 1)
	go func() { _, err := b.MigrateDown(ctx, 48); done <- err }()
	var worst time.Duration
	for end := time.Now().Add(2 * time.Second); time.Now().Before(end); time.Sleep(20 * time.Millisecond) {
		qctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		start := time.Now()
		var n int
		err := b.db.QueryRowContext(qctx, `SELECT count(*) FROM users WHERE id = 'nobody'`).Scan(&n)
		cancel()
		if err != nil {
			t.Fatalf("a read of users during the down: %v", err)
		}
		worst = max(worst, time.Since(start))
	}
	if err := reader.tx.Commit(); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("MigrateDown(48) once the reader ended: %v", err)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("MigrateDown(48) did not end once the reader ended")
	}
	if worst > time.Second {
		t.Errorf("a read of users waited %v behind the down's lock wait; want at most about its 300 ms lock_timeout", worst.Round(time.Millisecond))
	}
	if v, _ := b.CurrentSchemaVersion(ctx); v != 48 {
		t.Errorf("after the down the schema is at v%d, want 48", v)
	}
}
