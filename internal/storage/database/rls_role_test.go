package database

import (
	"context"
	"database/sql"
	"os"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestRLSBypassedByRole distinguishes the two connection shapes that decide
// whether this schema's Row-Level Security policies do anything at all.
//
// A superuser (or BYPASSRLS) role ignores RLS entirely — FORCE ROW LEVEL
// SECURITY does not help — so every policy is inert. Nothing observable in the
// running system says so: queries succeed and the policies still appear in the
// catalog. That is why startup warns.
//
// The test needs its own connections rather than openTestDB's, because the
// whole point is comparing two different roles.
func TestRLSBypassedByRole(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping RLS role-detection test")
	}
	ctx := context.Background()

	primary, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer primary.Close()
	if err := primary.PingContext(ctx); err != nil {
		t.Skipf("database unreachable: %v", err)
	}

	role, bypassed, known := rlsBypassedByRole(ctx, primary)
	if !known {
		t.Fatal("could not determine the connection role's RLS status")
	}
	t.Logf("DATABASE_URL role %q bypasses RLS: %v", role, bypassed)

	// Whatever the harness connects as, the predicate must agree with the
	// catalog rather than guessing.
	var want bool
	if err := primary.QueryRowContext(ctx,
		`SELECT rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`).Scan(&want); err != nil {
		t.Fatalf("catalog check: %v", err)
	}
	if bypassed != want {
		t.Errorf("rlsBypassedByRole = %v, catalog says %v", bypassed, want)
	}

	// An unreadable/closed connection must report "unknown", not a confident
	// "not bypassed" — a false negative here would silence the startup warning.
	closed, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	closed.Close()
	if _, _, known := rlsBypassedByRole(ctx, closed); known {
		t.Error("expected known=false on a closed connection")
	}
}
