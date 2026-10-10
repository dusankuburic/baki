package database

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

// The boot checks of New: they warn about a deployment the database cannot carry and never fail the boot.

// captureLog is a logger whose JSON lines the test reads back.
func captureLog() (*slog.Logger, *bytes.Buffer) {
	var buf bytes.Buffer
	return slog.New(slog.NewJSONHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})), &buf
}

// logLines decodes the captured JSON lines whose msg is msg.
func logLines(t *testing.T, buf *bytes.Buffer, msg string) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		if rec["msg"] == msg {
			out = append(out, rec)
		}
	}
	return out
}

// TestPoolCheck_WarnsOverServerCap is the arithmetic of the pool check against a fake of the server's settings:
// MaxOpenConns × max(replicas, 1) against max_connections less both reserves.
func TestPoolCheck_WarnsOverServerCap(t *testing.T) {
	azureB1ms := serverConnLimits{Max: 50, SuperuserReserved: 3} // 47 for the app
	pg16Reserved := serverConnLimits{Max: 100, SuperuserReserved: 3, Reserved: 10}
	for _, tc := range []struct {
		name              string
		limits            serverConnLimits
		maxOpen, replicas int
		warn              bool
		total, available  int
		suggested         int
	}{
		{name: "one replica fits", limits: azureB1ms, maxOpen: 25, replicas: 1},
		{name: "exactly what is left", limits: azureB1ms, maxOpen: 47, replicas: 1},
		{name: "one over", limits: azureB1ms, maxOpen: 48, replicas: 1, warn: true, total: 48, available: 47, suggested: 23},
		{name: "two replicas of the default", limits: azureB1ms, maxOpen: 25, replicas: 2, warn: true, total: 50, available: 47, suggested: 15},
		{name: "no replica count means one", limits: azureB1ms, maxOpen: 25, replicas: 0},
		{name: "no replica count, over alone", limits: azureB1ms, maxOpen: 60, replicas: 0, warn: true, total: 60, available: 47, suggested: 23},
		{name: "reserved_connections counts too", limits: pg16Reserved, maxOpen: 88, replicas: 1, warn: true, total: 88, available: 87, suggested: 43},
		{name: "an uncapped pool is not compared", limits: azureB1ms, maxOpen: 0, replicas: 3},
		{name: "a server with nothing left", limits: serverConnLimits{Max: 3, SuperuserReserved: 3}, maxOpen: 1, replicas: 1, warn: true, total: 1, available: 0, suggested: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, buf := captureLog()
			reads := 0
			read := func(context.Context) (serverConnLimits, error) { reads++; return tc.limits, nil }
			warnIfPoolExceedsServer(context.Background(), log, read, tc.maxOpen, tc.replicas, 0)

			lines := logLines(t, buf, LogPoolOverServerLimit)
			if !tc.warn {
				if len(lines) != 0 {
					t.Fatalf("warned, want quiet: %s", buf)
				}
				if tc.maxOpen <= 0 && reads != 0 {
					t.Errorf("read the server's settings for an uncapped pool")
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("want one %q line, got: %s", LogPoolOverServerLimit, buf)
			}
			rec := lines[0]
			if rec["level"] != "WARN" {
				t.Errorf("level = %v, want WARN", rec["level"])
			}
			for attr, want := range map[string]int{
				"max_open_conns":           tc.maxOpen,
				"pools_total":              tc.total,
				"server_max_connections":   tc.limits.Max,
				"available":                tc.available,
				"suggested_max_open_conns": tc.suggested,
			} {
				if got, _ := rec[attr].(float64); int(got) != want {
					t.Errorf("%s = %v, want %d", attr, rec[attr], want)
				}
			}
		})
	}
}

// TestPoolCheck_UnreadableSettingsStayQuiet: a server that does not answer is no reason to warn (or to fail).
func TestPoolCheck_UnreadableSettingsStayQuiet(t *testing.T) {
	log, buf := captureLog()
	read := func(context.Context) (serverConnLimits, error) {
		return serverConnLimits{}, errors.New("permission denied")
	}
	warnIfPoolExceedsServer(context.Background(), log, read, 25, 4, 0)
	if lines := logLines(t, buf, LogPoolOverServerLimit); len(lines) != 0 {
		t.Fatalf("warned without the server's settings: %s", buf)
	}
	if strings.Contains(buf.String(), `"level":"WARN"`) || strings.Contains(buf.String(), `"level":"ERROR"`) {
		t.Errorf("an unreadable setting must stay below WARN: %s", buf)
	}
}

// TestPoolCheck_ConfiguredServerCapWhenUnreadable: PAD_DB_SERVER_MAX_CONNS, the server's max_connections as the
// deployment knows it, stands in when the server's own settings cannot be read (a pooler in front, a restricted
// role). Its reserves are unknown, so the whole cap counts. The server's answer, when there is one, wins.
func TestPoolCheck_ConfiguredServerCapWhenUnreadable(t *testing.T) {
	unreadable := func(context.Context) (serverConnLimits, error) {
		return serverConnLimits{}, errors.New("permission denied")
	}
	readable := func(context.Context) (serverConnLimits, error) {
		return serverConnLimits{Max: 100, SuperuserReserved: 3}, nil
	}
	for _, tc := range []struct {
		name                          string
		read                          func(context.Context) (serverConnLimits, error)
		maxOpen, replicas, configured int
		warn                          bool
		serverMax, available          int
		source                        string
	}{
		{name: "unreadable, configured cap exceeded", read: unreadable, maxOpen: 25, replicas: 2, configured: 40,
			warn: true, serverMax: 40, available: 40, source: "PAD_DB_SERVER_MAX_CONNS"},
		{name: "unreadable, configured cap fits", read: unreadable, maxOpen: 25, replicas: 1, configured: 40},
		{name: "unreadable, nothing configured", read: unreadable, maxOpen: 25, replicas: 4},
		{name: "the server's answer wins over the configured cap", read: readable, maxOpen: 25, replicas: 2, configured: 20},
		{name: "the server's answer, exceeded", read: readable, maxOpen: 50, replicas: 2, configured: 1000,
			warn: true, serverMax: 100, available: 97, source: "server"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			log, buf := captureLog()
			warnIfPoolExceedsServer(context.Background(), log, tc.read, tc.maxOpen, tc.replicas, tc.configured)
			lines := logLines(t, buf, LogPoolOverServerLimit)
			if !tc.warn {
				if len(lines) != 0 {
					t.Fatalf("warned, want quiet: %s", buf)
				}
				return
			}
			if len(lines) != 1 {
				t.Fatalf("want one %q line, got: %s", LogPoolOverServerLimit, buf)
			}
			rec := lines[0]
			if got, _ := rec["server_max_connections"].(float64); int(got) != tc.serverMax {
				t.Errorf("server_max_connections = %v, want %d", rec["server_max_connections"], tc.serverMax)
			}
			if got, _ := rec["available"].(float64); int(got) != tc.available {
				t.Errorf("available = %v, want %d", rec["available"], tc.available)
			}
			if rec["limits_from"] != tc.source {
				t.Errorf("limits_from = %v, want %q", rec["limits_from"], tc.source)
			}
		})
	}
}

// TestDSN_PgBouncerPortWarns: port 6432 is where Azure Flexible Server's built-in PgBouncer listens, in
// transaction mode by default, which breaks the session advisory locks every Singleton job holds (ADR 0023). The
// port is read from the parsed config (every host of a multi-host DSN), and the DSN's password never reaches the
// log.
func TestDSN_PgBouncerPortWarns(t *testing.T) {
	for _, tc := range []struct {
		dsn  string
		warn bool
	}{
		{"postgres://baki_app:s3cret@pg.example.com:6432/baki?sslmode=require", true},
		{"host=pg.example.com port=6432 user=baki_app password=s3cret dbname=baki sslmode=require", true},
		{"postgres://baki_app:s3cret@pg-a.example.com:5432,pg-b.example.com:6432/baki?sslmode=require", true},
		{"postgres://baki_app:s3cret@pg.example.com:5432/baki?sslmode=require", false},
		{"postgres://baki_app:s3cret@pg.example.com/baki?sslmode=require", false},
		// The number elsewhere in the DSN is not the port.
		{"postgres://baki_app:s3cret@pg6432.example.com:5432/baki6432?sslmode=require&connect_timeout=6432", false},
	} {
		t.Run(tc.dsn, func(t *testing.T) {
			cfg, err := pgx.ParseConfig(tc.dsn)
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			log, buf := captureLog()
			warnIfPgBouncerPort(log, cfg)
			lines := logLines(t, buf, LogPgBouncerPort)
			if got := len(lines) == 1; got != tc.warn {
				t.Fatalf("warned = %v, want %v: %s", got, tc.warn, buf)
			}
			if tc.warn && lines[0]["level"] != "WARN" {
				t.Errorf("level = %v, want WARN", lines[0]["level"])
			}
			if strings.Contains(buf.String(), "s3cret") {
				t.Errorf("the DSN's password reached the log: %s", buf)
			}
		})
	}
}

// TestApplicationName_DSNWinsElseConfiguredElseDefault: pg_stat_activity and pg_locks name the replica that
// holds a connection (a job's lock, say), unless the operator's DSN already names it.
func TestApplicationName_DSNWinsElseConfiguredElseDefault(t *testing.T) {
	t.Setenv("PGAPPNAME", "") // pgconn reads it as the DSN's default
	for _, tc := range []struct{ dsn, configured, want string }{
		{"postgres://u:p@h:5432/db", "", DefaultApplicationName},
		{"postgres://u:p@h:5432/db", "baki-backend/replica-1", "baki-backend/replica-1"},
		{"postgres://u:p@h:5432/db?application_name=ops-psql", "baki-backend/replica-1", "ops-psql"},
		{"host=h dbname=db application_name=ops-psql", "", "ops-psql"},
	} {
		cfg, err := pgx.ParseConfig(tc.dsn)
		if err != nil {
			t.Fatalf("parse %q: %v", tc.dsn, err)
		}
		setApplicationName(cfg.RuntimeParams, tc.configured)
		if got := cfg.RuntimeParams["application_name"]; got != tc.want {
			t.Errorf("dsn %q, configured %q: application_name = %q, want %q", tc.dsn, tc.configured, got, tc.want)
		}
	}
	if DefaultApplicationName != "baki-backend" {
		t.Errorf("DefaultApplicationName = %q; operations.md and dashboards name baki-backend", DefaultApplicationName)
	}
}

// TestBicepPoolCeiling_IsTheAppDefault: infra/main.bicep caps the derived per-replica pool at the app's default
// size, which it states as a number of its own.
func TestBicepPoolCeiling_IsTheAppDefault(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("..", "..", "..", "infra", "main.bicep"))
	if err != nil {
		t.Fatalf("read main.bicep: %v", err)
	}
	m := regexp.MustCompile(`(?m)^var dbAppDefaultMaxOpenConns = (\d+)\b`).FindSubmatch(b)
	if m == nil {
		t.Fatal("main.bicep declares no `var dbAppDefaultMaxOpenConns = <n>`")
	}
	if n, _ := strconv.Atoi(string(m[1])); n != DefaultConfig("").MaxOpenConns {
		t.Errorf("main.bicep's dbAppDefaultMaxOpenConns = %d, DefaultConfig().MaxOpenConns = %d: keep them equal", n, DefaultConfig("").MaxOpenConns)
	}
}

// TestNew_BootChecksOnAServer runs the checks through New against a real server: the connections carry the
// application name (the DSN's wins), the server's limits read back as it reports them, and a pool the server
// cannot carry is reported without failing the boot.
func TestNew_BootChecksOnAServer(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set — skipping the boot checks against a server")
	}
	t.Setenv("PGAPPNAME", "")
	ctx := context.Background()
	open := func(t *testing.T, cfg Config) *PostgresStorageBackend {
		t.Helper()
		b, err := New(ctx, cfg)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		t.Cleanup(func() { _ = b.Close() })
		return b
	}
	appName := func(t *testing.T, b *PostgresStorageBackend) string {
		t.Helper()
		var name string
		if err := b.db.QueryRowContext(ctx, `SELECT application_name FROM pg_stat_activity WHERE pid = pg_backend_pid()`).Scan(&name); err != nil {
			t.Fatalf("read application_name: %v", err)
		}
		return name
	}

	t.Run("default name", func(t *testing.T) {
		if got := appName(t, open(t, DefaultConfig(dsn))); got != DefaultApplicationName {
			t.Errorf("application_name = %q, want %q", got, DefaultApplicationName)
		}
	})
	t.Run("configured name", func(t *testing.T) {
		cfg := DefaultConfig(dsn)
		cfg.ApplicationName = "baki-backend/test-replica"
		if got := appName(t, open(t, cfg)); got != "baki-backend/test-replica" {
			t.Errorf("application_name = %q, want the configured one", got)
		}
	})
	t.Run("the DSN's name wins", func(t *testing.T) {
		sep := "?"
		if strings.Contains(dsn, "?") {
			sep = "&"
		}
		cfg := DefaultConfig(dsn + sep + "application_name=ops-psql")
		cfg.ApplicationName = "baki-backend/test-replica"
		if got := appName(t, open(t, cfg)); got != "ops-psql" {
			t.Errorf("application_name = %q, want the DSN's ops-psql", got)
		}
	})
	t.Run("limits and the warning", func(t *testing.T) {
		b := open(t, DefaultConfig(dsn))
		lim, err := readServerConnLimits(ctx, b.db)
		if err != nil {
			t.Fatalf("readServerConnLimits: %v", err)
		}
		var show string
		if err := b.db.QueryRowContext(ctx, `SHOW max_connections`).Scan(&show); err != nil {
			t.Fatalf("SHOW max_connections: %v", err)
		}
		if strconv.Itoa(lim.Max) != show || lim.available() <= 0 || lim.available() > lim.Max {
			t.Fatalf("limits %+v do not match SHOW max_connections = %s", lim, show)
		}

		// A pool as big as the whole server, on two replicas: more than it can carry, and still a boot.
		log, buf := captureLog()
		prev := slog.Default()
		slog.SetDefault(log)
		t.Cleanup(func() { slog.SetDefault(prev) })
		cfg := DefaultConfig(dsn)
		cfg.MaxOpenConns = lim.Max
		cfg.ExpectedReplicas = 2
		open(t, cfg)
		slog.SetDefault(prev)
		lines := logLines(t, buf, LogPoolOverServerLimit)
		if len(lines) != 1 {
			t.Fatalf("want one %q line from New, got: %s", LogPoolOverServerLimit, buf)
		}
		if got, _ := lines[0]["pools_total"].(float64); int(got) != 2*lim.Max {
			t.Errorf("pools_total = %v, want %d", lines[0]["pools_total"], 2*lim.Max)
		}
	})
}
