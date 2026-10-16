package database

import (
	"context"
	"database/sql/driver"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestMigrations_SQLFilesLoad: the embedded step files parse and continue the Go list without a gap (the same
// checks that panic at init, surfaced as a test failure with the reason).
func TestMigrations_SQLFilesLoad(t *testing.T) {
	steps, err := loadSQLMigrations(sqlMigrationFS, "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if err := checkMigrationSequence(steps, goMigrations[len(goMigrations)-1].version); err != nil {
		t.Fatal(err)
	}
	if got, want := len(migrations), len(goMigrations)+len(steps); got != want {
		t.Fatalf("migrations has %d steps, want %d (Go list + .sql files)", got, want)
	}
}

func TestLoadSQLMigrations(t *testing.T) {
	const up = "-- baki:compat expand\nCREATE TABLE t (id int);\n"
	fsys := fstest.MapFS{
		"m/0032_add_t.up.sql":   {Data: []byte(up)},
		"m/0032_add_t.down.sql": {Data: []byte("DROP TABLE t;\n")},
		// BOM + CRLF must load (and hash) exactly like the LF text.
		"m/0033_add_u.up.sql": {Data: []byte("\xef\xbb\xbf-- baki:compat breaking\r\nALTER TABLE t DROP COLUMN id;\r\n")},
		"m/README.md":         {Data: []byte("ignored")},
	}
	steps, err := loadSQLMigrations(fsys, "m")
	if err != nil {
		t.Fatal(err)
	}
	if len(steps) != 2 {
		t.Fatalf("got %d steps, want 2", len(steps))
	}
	first, second := steps[0], steps[1]
	if first.version != 32 || first.name != "add_t" || first.compat != compatExpand || first.sql != up || first.downSQL != "DROP TABLE t;\n" {
		t.Errorf("v32 = %+v", first)
	}
	if second.version != 33 || second.compat != compatBreaking || second.downSQL != "" {
		t.Errorf("v33 = %+v (no down file must mean irreversible)", second)
	}
	if want := "-- baki:compat breaking\nALTER TABLE t DROP COLUMN id;\n"; second.sql != want {
		t.Errorf("v33 sql = %q, want BOM stripped and LF line endings %q", second.sql, want)
	}
	if err := checkMigrationSequence(steps, 31); err != nil {
		t.Errorf("sequence: %v", err)
	}
	if err := checkMigrationSequence(steps, 30); err == nil {
		t.Error("a gap after v30 must be refused")
	}
}

func TestLoadSQLMigrations_Rejects(t *testing.T) {
	cases := map[string]fstest.MapFS{
		"missing directive":      {"m/0032_a.up.sql": {Data: []byte("SELECT 1;")}},
		"two directives":         {"m/0032_a.up.sql": {Data: []byte("-- baki:compat expand\n-- baki:compat expand\nSELECT 1;")}},
		"unknown compat":         {"m/0032_a.up.sql": {Data: []byte("-- baki:compat maybe\nSELECT 1;")}},
		"bad file name":          {"m/32_a.up.sql": {Data: []byte("-- baki:compat expand\n")}},
		"upper-case name":        {"m/0032_Add.up.sql": {Data: []byte("-- baki:compat expand\n")}},
		"down without up":        {"m/0032_a.down.sql": {Data: []byte("SELECT 1;")}},
		"mismatched names":       {"m/0032_a.up.sql": {Data: []byte("-- baki:compat expand\n")}, "m/0032_b.down.sql": {Data: []byte("SELECT 1;")}},
		"two names, one version": {"m/0032_a.up.sql": {Data: []byte("-- baki:compat expand\n")}, "m/0032_b.up.sql": {Data: []byte("-- baki:compat expand\n")}},
	}
	for name, fsys := range cases {
		if _, err := loadSQLMigrations(fsys, "m"); err == nil {
			t.Errorf("%s: loaded without error", name)
		}
	}
}

// TestApplyMigration_RetriesOnLockTimeout: a step that cannot get a table lock within lock_timeout rolls back and
// is retried; it is recorded once, with its compat.
func TestApplyMigration_RetriesOnLockTimeout(t *testing.T) {
	defer func(backoff time.Duration) { migrationLockBackoff = backoff }(migrationLockBackoff)
	migrationLockBackoff = time.Millisecond

	var stepRuns atomic.Int32
	b, f := newFakeBackend(t, 1, func(query string, _ []driver.NamedValue) fakeResult {
		if strings.Contains(query, "CREATE TABLE retry_me") && stepRuns.Add(1) == 1 {
			return fakeResult{err: &pgconn.PgError{Code: pgLockNotAvailable, Message: "canceling statement due to lock timeout"}}
		}
		return fakeResult{}
	})
	ctx := context.Background()
	conn, err := b.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	m := migration{version: 32, name: "retry_me", sql: "CREATE TABLE retry_me (id int);", compat: compatExpand}
	if err := applyMigration(ctx, conn, m); err != nil {
		t.Fatalf("applyMigration: %v", err)
	}
	if n := stepRuns.Load(); n != 2 {
		t.Errorf("step ran %d times, want 2 (one lock timeout, one success)", n)
	}
	var setLocks, inserts int
	for _, c := range f.Calls() {
		switch {
		case strings.HasPrefix(c.query, "SET LOCAL lock_timeout"):
			setLocks++
		case strings.Contains(c.query, "INSERT INTO schema_migrations"):
			inserts++
			if len(c.args) != 4 || c.args[3].Value != "expand" {
				t.Errorf("recorded args = %v, want compat \"expand\" last", c.args)
			}
		}
	}
	if setLocks != 2 || inserts != 1 {
		t.Errorf("SET LOCAL lock_timeout x%d and INSERT x%d, want x2 and x1", setLocks, inserts)
	}
}

// TestApplyMigration_RetriesOnDeadlock: a step chosen as a deadlock's victim (a concurrent erasure or org delete locks
// the same tables in another order) rolls back and is retried like a lock timeout, instead of failing the boot.
func TestApplyMigration_RetriesOnDeadlock(t *testing.T) {
	defer func(backoff time.Duration) { migrationLockBackoff = backoff }(migrationLockBackoff)
	migrationLockBackoff = time.Millisecond

	var stepRuns atomic.Int32
	b, f := newFakeBackend(t, 1, func(query string, _ []driver.NamedValue) fakeResult {
		if strings.Contains(query, "ALTER TABLE deadlocked") && stepRuns.Add(1) <= 2 {
			return fakeResult{err: &pgconn.PgError{Code: "40P01", Message: "deadlock detected"}}
		}
		return fakeResult{}
	})
	ctx := context.Background()
	conn, err := b.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	m := migration{version: 32, name: "deadlocked", sql: "ALTER TABLE deadlocked ADD COLUMN x int;", compat: compatExpand}
	if err := applyMigration(ctx, conn, m); err != nil {
		t.Fatalf("applyMigration: %v", err)
	}
	if n := stepRuns.Load(); n != 3 {
		t.Errorf("step ran %d times, want 3 (two deadlocks, one success)", n)
	}
	inserts := 0
	for _, c := range f.Calls() {
		if strings.Contains(c.query, "INSERT INTO schema_migrations") {
			inserts++
		}
	}
	if inserts != 1 {
		t.Errorf("INSERT INTO schema_migrations x%d, want x1", inserts)
	}

	// The retries are bounded like the lock timeouts': a step that deadlocks every time fails the boot with the error.
	stepRuns.Store(-100)
	err = applyMigration(ctx, conn, m)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "40P01" {
		t.Fatalf("a step deadlocking every time: err = %v, want the deadlock", err)
	}
	if n := stepRuns.Load() + 100; n != int32(migrationLockAttempts) {
		t.Errorf("a step deadlocking every time ran %d times, want %d", n, migrationLockAttempts)
	}
}

// TestApplyMigration_OtherErrorsAreNotRetried: only lock timeouts and deadlocks are retried.
func TestApplyMigration_OtherErrorsAreNotRetried(t *testing.T) {
	var stepRuns atomic.Int32
	b, _ := newFakeBackend(t, 1, func(query string, _ []driver.NamedValue) fakeResult {
		if strings.Contains(query, "broken") {
			stepRuns.Add(1)
			return fakeResult{err: &pgconn.PgError{Code: "42601", Message: "syntax error"}}
		}
		return fakeResult{}
	})
	ctx := context.Background()
	conn, err := b.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	err = applyMigration(ctx, conn, migration{version: 32, name: "broken", sql: "broken"})
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42601" {
		t.Fatalf("err = %v, want the syntax error", err)
	}
	if n := stepRuns.Load(); n != 1 {
		t.Errorf("step ran %d times, want 1", n)
	}
}

func TestMigrationRecordedCompat(t *testing.T) {
	if got := (migration{}).recordedCompat(); got != compatBreaking {
		t.Errorf("zero compat records %q, want breaking", got)
	}
	if got := (migration{compat: compatExpand}).recordedCompat(); got != compatExpand {
		t.Errorf("expand records %q", got)
	}
}
