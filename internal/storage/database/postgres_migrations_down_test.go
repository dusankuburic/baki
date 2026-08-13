package database

import (
	"context"
	"os"
	"testing"
)

// TestMigrateDown_RoundTrip is the DATABASE_URL-gated integration test for the
// down-path: roll back the latest step, assert the recorded version dropped,
// then reconnect (forward migrate re-applies it) and assert we're back at the
// latest with no checksum drift. Proves the down-migration is a faithful inverse
// and the checksum gate survives a down→up cycle.
func TestMigrateDown_RoundTrip(t *testing.T) {
	b := openMigrationTestDB(t)
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
	b2, err := New(ctx, DefaultConfig(os.Getenv("DATABASE_URL")))
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
	b := openMigrationTestDB(t)
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
