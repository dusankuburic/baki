package database

import (
	"context"
	"strings"
	"testing"
)

// TestMigrate_AcceptsNewerExpandOnlySchema is the rollback that expand/contract exists for: a binary boots on a
// schema that is ahead of it only by steps recorded as expand, and still refuses once a newer step is breaking.
func TestMigrate_AcceptsNewerExpandOnlySchema(t *testing.T) {
	b, dsn := openIsolatedMigrationDB(t)
	ctx := context.Background()
	latest := migrations[len(migrations)-1].version

	if _, err := b.db.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, checksum, compat) VALUES ($1, 'adds_a_table', 'n/a', 'expand')`, latest+1); err != nil {
		t.Fatalf("seed expand step: %v", err)
	}
	b2, err := New(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("New() refused a schema ahead only by an expand step: %v", err)
	}
	b2.Close()

	if _, err := b.db.ExecContext(ctx,
		`INSERT INTO schema_migrations (version, name, checksum, compat) VALUES ($1, 'drops_a_column', 'n/a', 'breaking')`, latest+2); err != nil {
		t.Fatalf("seed breaking step: %v", err)
	}
	b3, err := New(ctx, DefaultConfig(dsn))
	if err == nil {
		b3.Close()
		t.Fatal("New() booted although a newer step is breaking")
	}
	if !strings.Contains(err.Error(), "drops_a_column") || strings.Contains(err.Error(), "adds_a_table") {
		t.Errorf("the refusal must name the breaking step only, got: %v", err)
	}
}

// TestMigrate_RecordsCompat: applied steps carry their compat, and the column exists on every schema.
func TestMigrate_RecordsCompat(t *testing.T) {
	b, _ := openIsolatedMigrationDB(t)
	rows, err := b.db.QueryContext(context.Background(), `SELECT version, compat FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("read compat: %v", err)
	}
	defer rows.Close()
	want := map[int]migrationCompat{}
	for _, m := range migrations {
		want[m.version] = m.recordedCompat()
	}
	for rows.Next() {
		var v int
		var c migrationCompat
		if err := rows.Scan(&v, &c); err != nil {
			t.Fatal(err)
		}
		if c != want[v] {
			t.Errorf("v%d recorded compat %q, want %q", v, c, want[v])
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
}
