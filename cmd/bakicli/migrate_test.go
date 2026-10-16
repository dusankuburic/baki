package main

import (
	"context"
	"os"
	"strings"
	"testing"

	"pad-analyzer/internal/storage/database"
	"pad-analyzer/internal/testutil/pgtest"
)

func TestBakicliMigrate_Usage(t *testing.T) {
	stdout, stderr, code := runBakicli(t, "migrate")
	if code != 2 {
		t.Errorf("expected exit code 2, got %d (stdout: %s, stderr: %s)", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "usage: bakicli migrate <subcommand>") {
		t.Errorf("expected migrate usage, got: %s", stderr)
	}
	if !strings.Contains(stderr, "adopt-checksum") || !strings.Contains(stderr, "preflight") {
		t.Errorf("expected subcommands in usage, got: %s", stderr)
	}
}

func TestBakicliMigrateDown_Args(t *testing.T) {
	_, stderr, code := runBakicli(t, "migrate", "down")
	if code != 2 {
		t.Errorf("expected exit code 2, got %d", code)
	}
	if !strings.Contains(stderr, "usage: bakicli migrate down") {
		t.Errorf("expected migrate down usage, got: %s", stderr)
	}

	_, stderr, code = runBakicli(t, "migrate", "down", "invalid")
	if code != 2 {
		t.Errorf("expected exit code 2 for invalid target, got %d", code)
	}
	if !strings.Contains(stderr, "non-negative integer") {
		t.Errorf("expected non-negative integer error, got: %s", stderr)
	}
}

func TestBakicliMigrateAdoptChecksum_Args(t *testing.T) {
	_, stderr, code := runBakicli(t, "migrate", "adopt-checksum")
	if code != 2 {
		t.Errorf("expected exit code 2, got %d", code)
	}
	if !strings.Contains(stderr, "usage: bakicli migrate adopt-checksum") {
		t.Errorf("expected adopt-checksum usage, got: %s", stderr)
	}

	_, stderr, code = runBakicli(t, "migrate", "adopt-checksum", "invalid")
	if code != 2 {
		t.Errorf("expected exit code 2 for invalid version, got %d", code)
	}
	if !strings.Contains(stderr, "positive integer") {
		t.Errorf("expected positive integer error, got: %s", stderr)
	}
}

func TestBakicliMigratePreflight_NoDSN(t *testing.T) {
	old := os.Getenv("PAD_DATABASE_URL")
	os.Unsetenv("PAD_DATABASE_URL")
	t.Cleanup(func() {
		if old != "" {
			os.Setenv("PAD_DATABASE_URL", old)
		}
	})

	_, stderr, code := runBakicli(t, "migrate", "preflight")
	if code != 2 {
		t.Errorf("expected exit code 2, got %d", code)
	}
	if !strings.Contains(stderr, "no database DSN") {
		t.Errorf("expected no database DSN error, got: %s", stderr)
	}
}

func openIsolatedMigrateCLIDB(t *testing.T) (*database.PostgresStorageBackend, string) {
	t.Helper()
	dsn := pgtest.NewDatabase(t, "")
	b, err := database.New(context.Background(), database.DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("migrate isolated db: %v", err)
	}
	t.Cleanup(func() { _ = b.Close() })
	return b, dsn
}

func TestBakicliMigrateDown_NewerVersionRefused(t *testing.T) {
	b, dsn := openIsolatedMigrateCLIDB(t)
	ctx := context.Background()
	_, err := b.DB().ExecContext(ctx, `INSERT INTO schema_migrations (version, name, checksum, compat)
		VALUES (9999, 'future_migration', 'fakechecksum', 'expand')`)
	if err != nil {
		t.Fatalf("insert version 9999: %v", err)
	}

	stdout, stderr, code := runBakicli(t, "migrate", "down", "--dsn", dsn, "--force", "47")
	if code != 2 {
		t.Fatalf("expected exit code 2 for newer version, got %d (stdout: %s, stderr: %s)", code, stdout, stderr)
	}
	if !strings.Contains(stderr, "v9999 is newer than this bakicli (knows v") {
		t.Errorf("expected newer version message in stderr, got: %s", stderr)
	}
}

func TestBakicliMigratePreflight_DuplicateEmails(t *testing.T) {
	b, dsn := openIsolatedMigrateCLIDB(t)
	ctx := context.Background()

	// Clean database passes preflight with exit code 0.
	stdout, stderr, code := runBakicli(t, "migrate", "preflight", "--dsn", dsn)
	if code != 0 {
		t.Fatalf("expected exit code 0 on clean DB, got %d (stdout: %s, stderr: %s)", code, stdout, stderr)
	}
	if !strings.Contains(stdout, "preflight: clean") {
		t.Errorf("expected clean message in stdout, got: %s", stdout)
	}

	// Roll back to v47 so case-colliding emails can be inserted as pre-0048 data.
	if _, err := b.MigrateDown(ctx, 47); err != nil {
		t.Fatalf("MigrateDown(47): %v", err)
	}

	id1 := "test-dup-1"
	id2 := "test-dup-2"
	_, err := b.DB().ExecContext(ctx, `INSERT INTO users (id, email, password, role, created_at)
		VALUES ($1, 'preflight-dup@example.com', '', 'member', NOW() - INTERVAL '2 hours'),
		       ($2, 'PREFLIGHT-DUP@EXAMPLE.COM', '', 'member', NOW() - INTERVAL '1 hour')`, id1, id2)
	if err != nil {
		t.Fatalf("insert duplicate users: %v", err)
	}

	stdout, _, code = runBakicli(t, "migrate", "preflight", "--dsn", dsn)
	if code != 1 {
		t.Fatalf("expected exit code 1 for preflight duplicates, got %d (stdout: %s)", code, stdout)
	}
	if !strings.Contains(stdout, id1) || !strings.Contains(stdout, id2) {
		t.Errorf("expected duplicate user IDs %s and %s in stdout, got: %s", id1, id2, stdout)
	}
	if !strings.Contains(stdout, "Recipe to resolve duplicates") {
		t.Errorf("expected recipe to be printed, got: %s", stdout)
	}
}
