package database

import (
	"context"
	"os"
	"strings"
	"testing"
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

// Tampering a recorded checksum (simulating an edited shipped migration) must
// make the next migrate() fail boot with a drift error.
func TestMigrate_ChecksumDriftFailsBoot(t *testing.T) {
	b := openMigrationTestDB(t)
	ctx := context.Background()

	// Snapshot v2's real checksum so we can restore it (the DB is shared).
	var original string
	if err := b.db.QueryRowContext(ctx,
		`SELECT checksum FROM schema_migrations WHERE version = 2`).Scan(&original); err != nil {
		t.Fatalf("read v2 checksum: %v", err)
	}
	t.Cleanup(func() {
		_, _ = b.db.ExecContext(context.Background(),
			`UPDATE schema_migrations SET checksum = $1 WHERE version = 2`, original)
	})

	if _, err := b.db.ExecContext(ctx,
		`UPDATE schema_migrations SET checksum = 'tampered' WHERE version = 2`); err != nil {
		t.Fatalf("tamper: %v", err)
	}

	err := b.migrate(ctx)
	if err == nil {
		t.Fatal("expected drift error, got nil")
	}
	if !strings.Contains(err.Error(), "drift") {
		t.Errorf("error should mention drift, got: %v", err)
	}
}

// A row with an empty checksum (pre-checksum deployment) must be backfilled with
// the current checksum on the next migrate(), not treated as drift.
func TestMigrate_BackfillsEmptyChecksum(t *testing.T) {
	b := openMigrationTestDB(t)
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
