package database

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"testing"

	"pad-analyzer/internal/testutil/pgtest"
)

// migrationChecksum must be deterministic and sensitive to any SQL change — no
// DB required.
func TestMigrationChecksum_Deterministic(t *testing.T) {
	const sqlA = "CREATE TABLE t (id INT);"
	first, second := migrationChecksum(sqlA), migrationChecksum(sqlA)
	if first != second {
		t.Error("checksum not deterministic for identical SQL")
	}
	if first == migrationChecksum(sqlA+" ") {
		t.Error("checksum should change when SQL changes (even whitespace)")
	}
	// SHA-256 hex is 64 chars.
	if len(first) != 64 {
		t.Errorf("checksum len = %d, want 64 (hex sha256)", len(first))
	}
}

// openMigrationTestDB connects to the podman DATABASE_URL harness (migrations
// run + checksums recorded on connect) or skips.
func openMigrationTestDB(t *testing.T) *PostgresStorageBackend {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping migration checksum integration test")
	}
	b, err := New(context.Background(), DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return b
}

// openIsolatedMigrationDB opens a private database cloned from a template migrated to this binary's schema, for
// tests that rewrite schema state (migrate down, tampered checksums, future-version rows): on the shared
// database those writes raced every other test binary booting against it. The DSN is for tests that boot a
// second backend against the same schema.
func openIsolatedMigrationDB(t *testing.T) (*PostgresStorageBackend, string) {
	t.Helper()
	dsn := pgtest.NewDatabase(t, migratedTemplate(t))
	b, err := New(context.Background(), DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { b.Close() })
	return b, dsn
}

// migratedTemplate is a database migrated to this binary's schema, built once per schema (keyed by the migration
// checksums) and cloned per test.
func migratedTemplate(t *testing.T) string {
	t.Helper()
	h := sha256.New()
	for _, m := range migrations {
		fmt.Fprintf(h, "%d %s\n", m.version, migrationChecksum(m.sql))
	}
	return pgtest.Template(t, "mig_"+hex.EncodeToString(h.Sum(nil))[:16], func(dsn string) error {
		b, err := New(context.Background(), DefaultConfig(dsn))
		if err != nil {
			return err
		}
		b.Close()
		return nil
	})
}

// After connect, every applied migration must carry the checksum of its current
// SQL — proving apply-time recording and the on-connect backfill both work.
func TestMigrate_RecordsChecksums(t *testing.T) {
	b := openMigrationTestDB(t)
	ctx := context.Background()
	for _, m := range migrations {
		var got string
		if err := b.db.QueryRowContext(ctx,
			`SELECT checksum FROM schema_migrations WHERE version = $1`, m.version).Scan(&got); err != nil {
			t.Fatalf("read checksum v%d: %v", m.version, err)
		}
		if want := migrationChecksum(m.sql); got != want {
			t.Errorf("v%d %q: checksum = %q, want %q", m.version, m.name, got, want)
		}
	}
}

// Tampering recorded checksums (simulating edited migrations) must make the next
// migrate() fail boot with a drift error naming every mismatched version.
func TestMigrate_ChecksumDriftFailsBoot(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()

	// Snapshot v2 and v3 real checksums so we can restore them (the DB is shared).
	var orig2, orig3 string
	if err := b.db.QueryRowContext(ctx,
		`SELECT checksum FROM schema_migrations WHERE version = 2`).Scan(&orig2); err != nil {
		t.Fatalf("read v2 checksum: %v", err)
	}
	if err := b.db.QueryRowContext(ctx,
		`SELECT checksum FROM schema_migrations WHERE version = 3`).Scan(&orig3); err != nil {
		t.Fatalf("read v3 checksum: %v", err)
	}
	t.Cleanup(func() {
		_, _ = b.db.ExecContext(context.Background(),
			`UPDATE schema_migrations SET checksum = $1 WHERE version = 2`, orig2)
		_, _ = b.db.ExecContext(context.Background(),
			`UPDATE schema_migrations SET checksum = $1 WHERE version = 3`, orig3)
	})

	if _, err := b.db.ExecContext(ctx,
		`UPDATE schema_migrations SET checksum = 'tampered' WHERE version IN (2, 3)`); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	err := b.migrate(ctx)
	if err == nil {
		t.Fatal("expected drift error, got nil")
	}
	if !strings.Contains(err.Error(), "drift") {
		t.Errorf("error should mention drift, got: %v", err)
	}
	if !strings.Contains(err.Error(), "v2") || !strings.Contains(err.Error(), "v3") {
		t.Errorf("error should name both drifted versions v2 and v3, got: %v", err)
	}
}

// TestMigrateDown_RunsOnADriftedChecksum verifies that a database with a drifted checksum
// can be opened with OpenForMaintenance and rolled back with MigrateDown, after which New
// re-applies the forward migration cleanly.
func TestMigrateDown_RunsOnADriftedChecksum(t *testing.T) {
	b, dsn := openIsolatedMigrationDB(t)
	ctx := context.Background()

	// Tamper v49's checksum
	if _, err := b.db.ExecContext(ctx,
		`UPDATE schema_migrations SET checksum = 'tampered_49' WHERE version = 49`); err != nil {
		t.Fatalf("tamper v49: %v", err)
	}

	// New() now fails with drift
	if _, err := New(ctx, DefaultConfig(dsn)); err == nil || !strings.Contains(err.Error(), "drift") {
		t.Fatalf("expected New() to fail with drift, got: %v", err)
	}

	// OpenForMaintenance succeeds and allows down migration
	maint, err := OpenForMaintenance(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("OpenForMaintenance failed: %v", err)
	}
	defer maint.Close()

	rolled, err := maint.MigrateDown(ctx, 48)
	if err != nil {
		t.Fatalf("MigrateDown(48) failed: %v", err)
	}
	if len(rolled) != 1 || rolled[0] != 49 {
		t.Fatalf("expected rolled back [49], got %v", rolled)
	}

	// New() can now boot and re-apply v49 with the valid checksum
	b2, err := New(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("New() after down failed: %v", err)
	}
	defer b2.Close()

	cur, err := b2.CurrentSchemaVersion(ctx)
	if err != nil || cur != 49 {
		t.Fatalf("expected schema version 49 after re-apply, got %d (err: %v)", cur, err)
	}
}

// TestAdoptChecksum verifies that AdoptChecksum updates the recorded checksum to match
// the binary's computed checksum, allowing New() to boot, and refuses invalid versions.
func TestAdoptChecksum(t *testing.T) {
	b, dsn := openIsolatedMigrationDB(t)
	ctx := context.Background()

	// Refuses versions above latest or unknown
	if _, _, err := b.AdoptChecksum(ctx, 9999); err == nil {
		t.Errorf("expected AdoptChecksum(9999) to fail")
	}
	if _, _, err := b.AdoptChecksum(ctx, -1); err == nil {
		t.Errorf("expected AdoptChecksum(-1) to fail")
	}

	// Tamper v49
	if _, err := b.db.ExecContext(ctx,
		`UPDATE schema_migrations SET checksum = 'drifted_49' WHERE version = 49`); err != nil {
		t.Fatalf("tamper v49: %v", err)
	}

	// Adopt checksum
	rec, comp, err := b.AdoptChecksum(ctx, 49)
	if err != nil {
		t.Fatalf("AdoptChecksum(49) failed: %v", err)
	}
	if rec != "drifted_49" {
		t.Errorf("recorded checksum mismatch: got %q, want %q", rec, "drifted_49")
	}
	expectedComp, _ := MigrationChecksum(49)
	if comp != expectedComp {
		t.Errorf("computed checksum mismatch: got %q, want %q", comp, expectedComp)
	}

	// New() now boots cleanly without drift
	b2, err := New(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("New() after AdoptChecksum failed: %v", err)
	}
	defer b2.Close()
}

// A row with an empty checksum (pre-checksum deployment) must be backfilled with
// the current checksum on the next migrate(), not treated as drift.
func TestMigrate_BackfillsEmptyChecksum(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	ctx := context.Background()

	var original string
	if err := b.db.QueryRowContext(ctx,
		`SELECT checksum FROM schema_migrations WHERE version = 3`).Scan(&original); err != nil {
		t.Fatalf("read v3 checksum: %v", err)
	}
	t.Cleanup(func() {
		_, _ = b.db.ExecContext(context.Background(),
			`UPDATE schema_migrations SET checksum = $1 WHERE version = 3`, original)
	})

	if _, err := b.db.ExecContext(ctx,
		`UPDATE schema_migrations SET checksum = '' WHERE version = 3`); err != nil {
		t.Fatalf("clear checksum: %v", err)
	}

	if err := b.migrate(ctx); err != nil {
		t.Fatalf("migrate should adopt empty checksum, got error: %v", err)
	}

	var got string
	if err := b.db.QueryRowContext(ctx,
		`SELECT checksum FROM schema_migrations WHERE version = 3`).Scan(&got); err != nil {
		t.Fatalf("re-read v3 checksum: %v", err)
	}
	if want := migrationChecksum(migrations[2].sql); got != want {
		t.Errorf("empty checksum not backfilled: got %q, want %q", got, want)
	}
}
