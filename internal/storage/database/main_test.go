package database

import (
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// liveComposeDatabase is the docker-compose database: the user's live data.
// CI and local throwaway runs use baki_app.
const liveComposeDatabase = "baki"

// TestMain refuses to run this package's tests against the live compose
// database. Two dozen test files here read DATABASE_URL directly, and some
// are destructive by design — TestMigrateDown_RoundTrip rolls the newest
// migration back on whatever database it is given, which since v25 drops
// the Azure model registry and deletes every non-default Azure resource's
// credential. One TestMain covers the whole test binary: the internal
// (package database) and external (package database_test) files build into
// one, and Go allows a single TestMain across both.
func TestMain(m *testing.M) {
	if msg := liveDatabaseRefusal(os.Getenv("DATABASE_URL")); msg != "" {
		fmt.Fprintln(os.Stderr, msg)
		os.Exit(2)
	}
	os.Exit(m.Run())
}

// liveDatabaseRefusal returns why dsn must not be tested against, or "" when
// it may. An unparseable DSN is left to the tests to report; a DSN without a
// database name connects to the database named after its user, as Postgres
// does.
func liveDatabaseRefusal(dsn string) string {
	if dsn == "" {
		return ""
	}
	cfg, err := pgconn.ParseConfig(dsn)
	if err != nil {
		return ""
	}
	db := cfg.Database
	if db == "" {
		db = cfg.User
	}
	if !strings.EqualFold(db, liveComposeDatabase) {
		return ""
	}
	return fmt.Sprintf("refusing to run the database tests: DATABASE_URL names the database %q, the live docker-compose database. "+
		"These tests write, and some roll migrations back (TestMigrateDown_RoundTrip). Point DATABASE_URL at a throwaway "+
		"database instead — CI uses %q.", db, "baki_app")
}

// TestLiveDatabaseRefusal pins the guard's reading of the DSN forms the
// tests are run with.
func TestLiveDatabaseRefusal(t *testing.T) {
	for _, tc := range []struct {
		dsn    string
		refuse bool
	}{
		{"", false},
		{"postgres://baki:secret@127.0.0.1:5432/baki?sslmode=disable", true},
		{"postgres://baki_app:baki_app@127.0.0.1:55432/baki_app?sslmode=disable", false},
		{"postgresql://someone@db.example:5432/BAKI", true},
		{"host=localhost port=5432 dbname=baki user=app sslmode=disable", true},
		{"host=localhost port=5432 dbname=baki_test user=baki sslmode=disable", false},
		// No database name: Postgres connects to the one named after the user.
		{"postgres://baki:secret@localhost:5432/?sslmode=disable", true},
		{"postgres://baki_app:x@localhost:55432/?sslmode=disable", false},
		// Unparseable: left to the tests to report.
		{"postgres://%zz", false},
	} {
		if got := liveDatabaseRefusal(tc.dsn) != ""; got != tc.refuse {
			t.Errorf("liveDatabaseRefusal(%q) refuses = %v, want %v", tc.dsn, got, tc.refuse)
		}
	}
}
