package database

import "testing"

// TestMigrations_Ordered guards the migration set's invariants so an
// accidentally reordered/edited/gap-introducing change fails fast in any
// environment (no DB required). Versions must be 1..N, unique, ascending, and
// every step must carry non-empty SQL — a blank step would silently "succeed"
// and advance the recorded version past real work.
func TestMigrations_Ordered(t *testing.T) {
	if len(migrations) == 0 {
		t.Fatal("migrations must not be empty")
	}
	seen := map[int]string{}
	for i, m := range migrations {
		if m.version != i+1 {
			t.Errorf("migration[%d].version = %d, want %d (must be contiguous from 1)", i, m.version, i+1)
		}
		if m.name == "" {
			t.Errorf("migration v%d has empty name", m.version)
		}
		if prev, dup := seen[m.version]; dup {
			t.Errorf("migration version %d duplicated (%q and %q)", m.version, prev, m.name)
		}
		seen[m.version] = m.name
		if m.sql == "" {
			t.Errorf("migration v%d %q has empty SQL", m.version, m.name)
		}
	}
	// Baseline is always v1 and references the original schema.
	if migrations[0].version != 1 || migrations[0].name != "baseline" {
		t.Errorf("baseline must be v1 \"baseline\", got v%d %q", migrations[0].version, migrations[0].name)
	}
	if migrations[0].sql != schemaBaseline {
		t.Error("baseline migration must reference schemaBaseline verbatim")
	}
}
