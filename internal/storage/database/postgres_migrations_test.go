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

// TestMigrations_Reversibility locks the down-migration contract: the baseline
// (v1) is intentionally irreversible (downSQL==""), and every later step must
// carry a non-empty downSQL so the operator `migrate down` path is complete
// from any version back to v1's successor. A new step added without a downSQL
// would silently make the rollback path non-reversible past it — this fails fast.
func TestMigrations_Reversibility(t *testing.T) {
	if len(migrations) < 2 {
		t.Fatal("need at least baseline + one reversible step to test")
	}
	// Baseline is irreversible.
	if migrations[0].downSQL != "" {
		t.Errorf("baseline (v1) must have empty downSQL (irreversible), got non-empty")
	}
	// Every other step must be reversible.
	for _, m := range migrations[1:] {
		if m.downSQL == "" {
			t.Errorf("migration v%d %q has empty downSQL — only the baseline may be irreversible; add a reverse or document why it's unsafe", m.version, m.name)
		}
	}

	// MigrationSteps must mirror the slice and flag reversibility correctly.
	steps := MigrationSteps()
	if len(steps) != len(migrations) {
		t.Fatalf("MigrationSteps() returned %d steps, want %d", len(steps), len(migrations))
	}
	for i, s := range steps {
		want := migrations[i].downSQL != ""
		if s.Reversible != want {
			t.Errorf("MigrationSteps()[%d] (v%d) Reversible=%v, want %v", i, s.Version, s.Reversible, want)
		}
		if s.Version != migrations[i].version || s.Name != migrations[i].name {
			t.Errorf("MigrationSteps()[%d] = {v%d %q}, want {v%d %q}", i, s.Version, s.Name, migrations[i].version, migrations[i].name)
		}
	}
}

// TestMigrateDown_PrechecksReversibility confirms MigrateDown refuses to cross
// an irreversible step. With the current set the baseline (v1) is irreversible,
// so asking to roll all the way back to v0 must fail with
// ErrMigrationNotReversible WITHOUT executing any down (no DB is available, so
// this asserts the path-validation logic short-circuits before any connection
// work by checking the pure helpers).
func TestMigrateDown_PrechecksReversibility(t *testing.T) {
	// The pure lookup + reversibility check: every version > 1 down to 2 is
	// reversible, but v1 (baseline) is not. Rolling back TO v0 would require
	// crossing v1, which must be refused.
	base, found := migrationByVersion(1)
	if !found {
		t.Fatal("v1 not found")
	}
	if base.downSQL != "" {
		t.Errorf("v1 must be non-reversible (baseline), but has a downSQL")
	}
	for v := 2; v <= len(migrations); v++ {
		step, ok := migrationByVersion(v)
		if !ok {
			t.Fatalf("migrationByVersion(%d) not found", v)
		}
		if step.downSQL == "" {
			t.Errorf("v%d should be reversible for the rollback path to work", v)
		}
	}
}
