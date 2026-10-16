package database

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"

	"pad-analyzer/internal/testutil/pgtest"
)

// TestRLSBypassedByRole distinguishes the two connection shapes that decide
// whether this schema's Row-Level Security policies do anything at all.
//
// A superuser (or BYPASSRLS) role ignores RLS entirely — FORCE ROW LEVEL
// SECURITY does not help — so every policy is inert. Nothing observable in the
// running system says so: queries succeed and the policies still appear in the
// catalog. That is why startup warns (and, in the cloud profile, refuses).
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

	role, bypassed, err := rlsBypassedByRole(ctx, primary)
	if err != nil {
		t.Fatalf("could not determine the connection role's RLS status: %v", err)
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

	// An unreadable/closed connection must report an error, not a confident
	// "not bypassed" — a false negative here would silence the startup check.
	closed, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	closed.Close()
	if _, _, err := rlsBypassedByRole(ctx, closed); err == nil {
		t.Error("expected an error on a closed connection")
	}
}

// TestCheckRLSRole_RefusesOrWarns: a connection role that bypasses Row-Level Security makes every tenant policy
// inert. Asked to refuse (the cloud profile), the boot fails naming the role and the escape hatch; otherwise it
// warns, as it always did. A role the check cannot read refuses too when asked to (it cannot tell the policies hold)
// and stays quiet otherwise; an ordinary role passes silently either way.
func TestCheckRLSRole_RefusesOrWarns(t *testing.T) {
	reads := func(role string, bypassed bool, err error) func(context.Context) (string, bool, error) {
		return func(context.Context) (string, bool, error) { return role, bypassed, err }
	}
	unreadable := errors.New("connection refused")
	for _, tc := range []struct {
		name            string
		read            func(context.Context) (string, bool, error)
		refuse, wantErr bool
		wantWarn        bool
		errMentions     []string
	}{
		{"bypassing, refused", reads("postgres", true, nil), true, true, false, []string{`"postgres"`, "PAD_DB_ALLOW_BYPASSRLS"}},
		{"bypassing, warned", reads("postgres", true, nil), false, false, true, nil},
		{"ordinary, refuse mode", reads("baki_app", false, nil), true, false, false, nil},
		{"ordinary, warn mode", reads("baki_app", false, nil), false, false, false, nil},
		{"unreadable, refused with the cause", reads("", false, unreadable), true, true, false, []string{"PAD_DB_ALLOW_BYPASSRLS", "connection refused"}},
		{"unreadable, warn mode", reads("", false, unreadable), false, false, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, buf := captureLog()
			err := checkRLSRole(context.Background(), log, tc.read, tc.refuse)
			if (err != nil) != tc.wantErr {
				t.Fatalf("checkRLSRole = %v, want an error: %v", err, tc.wantErr)
			}
			for _, m := range tc.errMentions {
				if !strings.Contains(err.Error(), m) {
					t.Errorf("error %q does not mention %s", err, m)
				}
			}
			if warned := len(logLines(t, buf, LogRLSPoliciesInert)) == 1; warned != tc.wantWarn {
				t.Errorf("warned = %v, want %v; log: %s", warned, tc.wantWarn, buf)
			}
		})
	}
}

// TestNew_RoleCheckOnAServer runs the check through New against a real server, asked to refuse as the cloud profile
// asks: the DATABASE_URL role boots when it is an ordinary one (CI's NOSUPERUSER NOBYPASSRLS role) and is refused
// when it bypasses (a superuser DSN), and then before the migrations have created anything in the fresh database.
func TestNew_RoleCheckOnAServer(t *testing.T) {
	dsn := pgtest.NewDatabase(t, "") // a fresh, unmigrated database; skips without DATABASE_URL
	ctx := context.Background()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	role, bypassed, err := rlsBypassedByRole(ctx, db)
	if err != nil {
		t.Fatalf("could not read the DATABASE_URL role: %v", err)
	}

	cfg := DefaultConfig(dsn)
	cfg.RefuseRLSBypass = true
	b, err := New(ctx, cfg)
	if !bypassed {
		if err != nil {
			t.Fatalf("New refused the ordinary role %q: %v", role, err)
		}
		_ = b.Close()
		return
	}
	t.Logf("DATABASE_URL role %q bypasses RLS: checking the refusal", role)
	if err == nil {
		_ = b.Close()
		t.Fatalf("New booted on the bypassing role %q with RefuseRLSBypass", role)
	}
	if !strings.Contains(err.Error(), "PAD_DB_ALLOW_BYPASSRLS") {
		t.Errorf("refusal %q does not name PAD_DB_ALLOW_BYPASSRLS", err)
	}
	var migrated bool
	if err := db.QueryRowContext(ctx, `SELECT to_regclass('schema_migrations') IS NOT NULL`).Scan(&migrated); err != nil {
		t.Fatal(err)
	}
	if migrated {
		t.Error("the refused boot migrated the database first: tables owned by the bypassing role")
	}
}
