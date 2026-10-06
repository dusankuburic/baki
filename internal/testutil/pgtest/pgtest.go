// Package pgtest gives Postgres-backed tests a database of their own on the DATABASE_URL server, so a test that
// rewrites schema state (migrate down, tampered checksums, future-version rows) cannot disturb the other test
// binaries running against the same server at the same time.
//
// The test role needs CREATEDB (CI grants it; it does not bypass RLS). Databases are cloned from a template, so
// the base template (PAD_TEST_TEMPLATE_DB, default "baki_template" when it exists) should carry the extensions
// the unprivileged role cannot install itself, such as vector.
//
// The package does not import the storage layer: Template takes the preparation step (usually "run the
// migrations") as a callback, which keeps it usable from that package's own tests.
package pgtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"hash/fnv"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

// defaultBaseTemplate is the database CI creates with the vector extension installed.
const defaultBaseTemplate = "baki_template"

// DSN returns DATABASE_URL, skipping the test when it is unset.
func DSN(t testing.TB) string {
	t.Helper()
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping Postgres test")
	}
	return dsn
}

// NewDatabase creates a database cloned from template ("" = the base template) and drops it when the test ends.
// It returns the new database's DSN.
func NewDatabase(t testing.TB, template string) string {
	t.Helper()
	dsn := DSN(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	admin := connect(t, ctx, dsn)
	defer func() { _ = admin.Close(context.Background()) }()

	if template == "" {
		template = baseTemplate(t, ctx, admin)
	}
	name := "baki_t_" + randomHex(t, 6)
	if err := createDatabase(ctx, admin, name, template); err != nil {
		t.Fatalf("pgtest: create database %s: %v%s", name, err, privilegeHint(err))
	}
	t.Cleanup(func() { dropDatabase(t, dsn, name) })
	return withDatabase(t, dsn, name)
}

// Template returns the name of a database prepared once per key: created from the base template, handed to
// prepare (usually "run the migrations"), then renamed into place, so a half-prepared template is never cloned.
// It is safe across concurrently running test binaries (an advisory lock) and reused by later runs with the
// same key, so the key should identify what prepare produces (e.g. a hash of the migration checksums).
func Template(t testing.TB, key string, prepare func(dsn string) error) string {
	t.Helper()
	dsn := DSN(t)
	name := "baki_tpl_" + sanitizeKey(key)

	templatesMu.Lock()
	defer templatesMu.Unlock()
	if templates[name] {
		return name
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	admin := connect(t, ctx, dsn)
	defer func() { _ = admin.Close(context.Background()) }()

	lockKey := advisoryKey(name)
	if _, err := admin.Exec(ctx, `SELECT pg_advisory_lock($1)`, lockKey); err != nil {
		t.Fatalf("pgtest: lock template %s: %v", name, err)
	}
	defer func() { _, _ = admin.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, lockKey) }()

	exists, err := databaseExists(ctx, admin, name)
	if err != nil {
		t.Fatalf("pgtest: look up template %s: %v", name, err)
	}
	if !exists {
		building := name + "_b" + randomHex(t, 3)
		if err := createDatabase(ctx, admin, building, baseTemplate(t, ctx, admin)); err != nil {
			t.Fatalf("pgtest: create template %s: %v%s", building, err, privilegeHint(err))
		}
		if err := prepare(withDatabase(t, dsn, building)); err != nil {
			dropDatabase(t, dsn, building)
			t.Fatalf("pgtest: prepare template %s: %v", name, err)
		}
		// Renaming (like cloning) needs the database to be idle; prepare closed its pool, but a backend can
		// still be on its way out.
		_, _ = admin.Exec(ctx, `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname = $1 AND pid <> pg_backend_pid()`, building)
		if _, err := admin.Exec(ctx, fmt.Sprintf(`ALTER DATABASE %s RENAME TO %s`, ident(building), ident(name))); err != nil {
			dropDatabase(t, dsn, building)
			t.Fatalf("pgtest: publish template %s: %v", name, err)
		}
	}
	templates[name] = true
	return name
}

var (
	templatesMu sync.Mutex
	templates   = map[string]bool{} // templates known to exist on the server, per process
)

func connect(t testing.TB, ctx context.Context, dsn string) *pgx.Conn {
	t.Helper()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatalf("pgtest: connect: %v", err)
	}
	return conn
}

// baseTemplate resolves PAD_TEST_TEMPLATE_DB, else "baki_template" when the server has it, else "" (the
// server's default template, without the vector extension).
func baseTemplate(t testing.TB, ctx context.Context, admin *pgx.Conn) string {
	t.Helper()
	if name := os.Getenv("PAD_TEST_TEMPLATE_DB"); name != "" {
		return name
	}
	exists, err := databaseExists(ctx, admin, defaultBaseTemplate)
	if err != nil {
		t.Fatalf("pgtest: look up %s: %v", defaultBaseTemplate, err)
	}
	if exists {
		return defaultBaseTemplate
	}
	return ""
}

func databaseExists(ctx context.Context, admin *pgx.Conn, name string) (bool, error) {
	var exists bool
	err := admin.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM pg_database WHERE datname = $1)`, name).Scan(&exists)
	return exists, err
}

func createDatabase(ctx context.Context, admin *pgx.Conn, name, template string) error {
	stmt := "CREATE DATABASE " + ident(name)
	if template != "" {
		stmt += " TEMPLATE " + ident(template)
	}
	_, err := admin.Exec(ctx, stmt)
	return err
}

// dropDatabase removes name on a fresh connection; WITH (FORCE) ends connections a test left open.
func dropDatabase(t testing.TB, dsn, name string) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Logf("pgtest: drop %s: connect: %v", name, err)
		return
	}
	defer func() { _ = conn.Close(context.Background()) }()
	if _, err := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+ident(name)+" WITH (FORCE)"); err != nil {
		t.Logf("pgtest: drop %s: %v", name, err)
	}
}

// withDatabase returns dsn pointed at database name (URL or keyword/value form).
func withDatabase(t testing.TB, dsn, name string) string {
	t.Helper()
	if strings.HasPrefix(dsn, "postgres://") || strings.HasPrefix(dsn, "postgresql://") {
		u, err := url.Parse(dsn)
		if err != nil {
			t.Fatalf("pgtest: parse DATABASE_URL: %v", err)
		}
		u.Path = "/" + name
		return u.String()
	}
	return dsn + " dbname=" + name // later keys win in keyword/value DSNs
}

func privilegeHint(err error) string {
	if err != nil && strings.Contains(err.Error(), "permission denied") {
		return " (the test role needs CREATEDB: ALTER ROLE <role> CREATEDB)"
	}
	return ""
}

func ident(name string) string { return pgx.Identifier{name}.Sanitize() }

func advisoryKey(name string) int64 {
	h := fnv.New32a()
	_, _ = h.Write([]byte("pgtest:" + name))
	return int64(h.Sum32())
}

// sanitizeKey keeps database names valid and within Postgres' 63-byte identifier limit.
func sanitizeKey(key string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(key) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '_' {
			b.WriteRune(r)
		}
	}
	s := b.String()
	if s == "" {
		s = "default"
	}
	if len(s) > 40 {
		s = s[:40]
	}
	return s
}

func randomHex(t testing.TB, n int) string {
	t.Helper()
	buf := make([]byte, n)
	if _, err := rand.Read(buf); err != nil {
		t.Fatalf("pgtest: random name: %v", err)
	}
	return hex.EncodeToString(buf)
}
