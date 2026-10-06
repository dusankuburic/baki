package database

import (
	"context"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// perfRound4V29Checksum pins v29's SQL. Once v29 has shipped, a change to
// its text fails every deployed database's boot (verifyChecksums): fix
// forward with a v30 instead of editing v29.
const perfRound4V29Checksum = "7615c4354d28d2eb2ee53dc37f8863f34effbe31be07aa050119af808d2bdc73"

// v29 catalog objects.
var (
	v29Index = "flows_lower_name_updated_idx"
	// v29Compressed are the (table, column) pairs v29 switches to lz4 TOAST
	// compression (and its down step returns to pglz).
	v29Compressed = []struct{ table, column string }{
		{"flows", "content"},
		{"flow_search", "body"},
		{"flow_versions", "content"},
		{"conversations", "messages"},
	}
)

// TestMigration29_Shape pins the v29 step without a database: the name-sort
// index whose columns are exactly the name sort's ORDER BY, the lz4 block
// for the four wide columns (guarded so a server without lz4 keeps pglz),
// no CONCURRENTLY — and a down step that drops the index and restores pglz.
func TestMigration29_Shape(t *testing.T) {
	m, ok := migrationByVersion(29)
	if !ok {
		t.Fatal("migration v29 not registered")
	}
	if m.name != "perf_round4_v29" {
		t.Errorf("v29 name = %q, want perf_round4_v29", m.name)
	}
	sqlText := oneLine(m.sql)
	for _, want := range []string{
		"CREATE INDEX IF NOT EXISTS flows_lower_name_updated_idx ON flows (lower(name), updated_at DESC, id);",
		"EXCEPTION WHEN feature_not_supported THEN RAISE NOTICE",
	} {
		if !strings.Contains(sqlText, want) {
			t.Errorf("v29 lacks %q", want)
		}
	}
	for _, c := range v29Compressed {
		if want := "ALTER TABLE " + c.table + " ALTER COLUMN " + c.column + " SET COMPRESSION lz4;"; !strings.Contains(sqlText, want) {
			t.Errorf("v29 lacks %q", want)
		}
	}
	if strings.Contains(strings.ToUpper(m.sql), "CONCURRENTLY") {
		t.Error("v29 must not use CONCURRENTLY: applyMigration runs each step in a transaction")
	}
	if strings.Count(strings.ToUpper(m.sql), "CREATE INDEX") != strings.Count(strings.ToUpper(m.sql), "CREATE INDEX IF NOT EXISTS") {
		t.Error("every v29 CREATE INDEX must be IF NOT EXISTS")
	}
	// The index serves the name sort only if its columns ARE the ORDER BY
	// (flowOrderBy): a change to either side without the other is a silent
	// return to sorting every page.
	order := strings.TrimPrefix(flowOrderBy(interfaces.FlowSortNameAsc), "ORDER BY ")
	if got := strings.ReplaceAll(order, " ASC", ""); got != "lower(name), updated_at DESC, id" {
		t.Errorf("FlowSortNameAsc orders by %q; flows_lower_name_updated_idx is (lower(name), updated_at DESC, id) — change both or neither", order)
	}

	down := oneLine(m.downSQL)
	if !strings.Contains(down, "DROP INDEX IF EXISTS flows_lower_name_updated_idx;") {
		t.Error("v29 down does not drop flows_lower_name_updated_idx")
	}
	for _, c := range v29Compressed {
		if want := "ALTER TABLE " + c.table + " ALTER COLUMN " + c.column + " SET COMPRESSION pglz;"; !strings.Contains(down, want) {
			t.Errorf("v29 down lacks %q", want)
		}
	}
}

// TestMigration29_ChecksumPinned fails when v29's SQL text changes.
func TestMigration29_ChecksumPinned(t *testing.T) {
	m, _ := migrationByVersion(29)
	if got := migrationChecksum(m.sql); got != perfRound4V29Checksum {
		t.Errorf("v29 checksum = %s, want the pinned %s — a shipped migration must never change; add v30 instead", got, perfRound4V29Checksum)
	}
}

// columnCompression returns pg_attribute.attcompression for table.column:
// 'l' (lz4), 'p' (pglz) or "" (the server default).
func columnCompression(t *testing.T, b *PostgresStorageBackend, table, column string) string {
	t.Helper()
	var c string
	if err := b.db.QueryRowContext(context.Background(), `
		SELECT attcompression::text FROM pg_attribute WHERE attrelid = $1::regclass AND attname = $2`, table, column).Scan(&c); err != nil {
		t.Fatalf("pg_attribute %s.%s: %v", table, column, err)
	}
	return c
}

// assertV29Applied checks the catalog for the v29 end state (or its absence).
func assertV29Applied(t *testing.T, b *PostgresStorageBackend, applied bool) {
	t.Helper()
	if have := catalogIndexes(t, b, []string{v29Index}); have[v29Index] != applied {
		t.Errorf("index %s exists = %v, want %v", v29Index, have[v29Index], applied)
	}
	want := "l"
	if !applied {
		want = "p"
	}
	for _, c := range v29Compressed {
		if got := columnCompression(t, b, c.table, c.column); got != want {
			t.Errorf("%s.%s attcompression = %q, want %q", c.table, c.column, got, want)
		}
	}
}

// r4SaveWideFlow saves a flow whose content is wide and compressible enough
// to be TOAST-compressed (pg_column_compression reports NULL for a value
// stored uncompressed, i.e. anything under the ~2 KB threshold) and returns
// the compression method recorded on the stored flows.content and
// flow_search.body values.
func r4SaveWideFlow(t *testing.T, b *PostgresStorageBackend, id string) (content, body string) {
	t.Helper()
	ctx := context.Background()
	t.Cleanup(func() { _ = b.DeleteFlow(ctx, id) })
	source := strings.Repeat("Display.ShowMessageBox Message: 'compress me' Title: 'lz4'\\n", 2000)
	if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: id, Name: "wide", OwnerID: "r4-mig-owner", Content: []byte(`{"source":"` + source + `"}`)}); err != nil {
		t.Fatalf("SaveFlow %s: %v", id, err)
	}
	var c, s *string
	if err := b.db.QueryRowContext(ctx, `
		SELECT pg_column_compression(f.content), pg_column_compression(s.body)
		FROM flows f JOIN flow_search s ON s.flow_id = f.id WHERE f.id = $1`, id).Scan(&c, &s); err != nil {
		t.Fatalf("pg_column_compression %s: %v", id, err)
	}
	deref := func(p *string) string {
		if p == nil {
			return "<uncompressed>"
		}
		return *p
	}
	return deref(c), deref(s)
}

// TestMigration29_UpDownUp is the DATABASE_URL-gated round trip: v29 applied
// (index, lz4 on the four columns, checksum recorded; a freshly saved flow's
// content and search body are stored lz4-compressed), rolled back (index
// gone, pglz again, new values stored pglz), re-applied.
func TestMigration29_UpDownUp(t *testing.T) {
	b, dsn := openIsolatedMigrationDB(t)
	ctx := context.Background()
	current, err := b.CurrentSchemaVersion(ctx)
	if err != nil {
		t.Fatalf("read version: %v", err)
	}
	if current < 29 {
		t.Skipf("DB at v%d; another test may have rolled it back", current)
	}
	assertV29Applied(t, b, true)
	var sum string
	if err := b.db.QueryRowContext(ctx, `SELECT checksum FROM schema_migrations WHERE version = 29`).Scan(&sum); err != nil {
		t.Fatalf("read v29 checksum: %v", err)
	}
	if sum != perfRound4V29Checksum {
		t.Errorf("recorded v29 checksum = %s, want %s", sum, perfRound4V29Checksum)
	}
	stamp := time.Now().Format("150405.000000000")
	if c, s := r4SaveWideFlow(t, b, "r4-mig-lz4-"+stamp); c != "lz4" || s != "lz4" {
		t.Errorf("at v29 a saved flow is stored content=%s body=%s, want lz4/lz4", c, s)
	}

	// Down. Later rounds append migrations above v29; MigrateDown(28) rolls
	// them all back, newest first, and v29 must be the last one rolled.
	rolled, err := b.MigrateDown(ctx, 28)
	if err != nil {
		t.Fatalf("MigrateDown(28): %v", err)
	}
	if len(rolled) == 0 || rolled[len(rolled)-1] != 29 {
		t.Fatalf("rolled back %v, want a list ending in 29", rolled)
	}
	assertV29Applied(t, b, false)
	if c, s := r4SaveWideFlow(t, b, "r4-mig-pglz-"+stamp); c != "pglz" || s != "pglz" {
		t.Errorf("at v28 a saved flow is stored content=%s body=%s, want pglz/pglz", c, s)
	}

	// Up again: boot-time migrate on reconnect.
	b2, err := New(ctx, DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("reconnect (re-apply v29): %v", err)
	}
	// A cleanup, not a defer: the wide flow saved through b2 below is
	// removed by a cleanup registered after this one, which must still find
	// b2 open (cleanups run last-registered first; a defer would close it
	// before any of them).
	t.Cleanup(func() { b2.Close() })
	if v, _ := b2.CurrentSchemaVersion(ctx); v < 29 {
		t.Fatalf("after re-apply, version = %d, want >= 29", v)
	}
	assertV29Applied(t, b2, true)
	if c, s := r4SaveWideFlow(t, b2, "r4-mig-lz4-again-"+stamp); c != "lz4" || s != "lz4" {
		t.Errorf("after the re-apply a saved flow is stored content=%s body=%s, want lz4/lz4", c, s)
	}
}
