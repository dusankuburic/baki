package app

import (
	"context"
	"testing"
	"time"

	"go.uber.org/fx"
	"go.uber.org/fx/fxtest"

	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/platform/lock"
	"pad-analyzer/internal/platform/secret"
	storagedb "pad-analyzer/internal/storage/database"
	"pad-analyzer/internal/testutil/pgtest"
)

// TestStorageConfig_DefaultsAreTheBackends: config.Default holds the Postgres backend's own defaults for the pool
// durations and the statement timeout (so an explicit "0s" and unset differ); they must stay the backend's.
func TestStorageConfig_DefaultsAreTheBackends(t *testing.T) {
	cfg, backend := config.Default(), storagedb.DefaultConfig("")
	for _, tc := range []struct {
		name           string
		config, theirs time.Duration
	}{
		{"DBConnMaxLifetime", cfg.Storage.DBConnMaxLifetime.Std(), backend.ConnMaxLifetime},
		{"DBConnMaxIdleTime", cfg.Storage.DBConnMaxIdleTime.Std(), backend.ConnMaxIdleTime},
		{"DBStatementTimeout", cfg.Storage.DBStatementTimeout.Std(), backend.StatementTimeout},
	} {
		if tc.config != tc.theirs {
			t.Errorf("config.Default().Storage.%s = %v, storagedb.DefaultConfig = %v: keep them equal", tc.name, tc.config, tc.theirs)
		}
	}
	// So the default configuration builds the backend's default pool.
	got := storageConfig(cfg)
	if got.ConnMaxLifetime != backend.ConnMaxLifetime || got.ConnMaxIdleTime != backend.ConnMaxIdleTime ||
		got.StatementTimeout != backend.StatementTimeout || got.MaxOpenConns != backend.MaxOpenConns {
		t.Errorf("storageConfig(default) = %+v, want the backend's defaults %+v", got, backend)
	}
}

// TestStorageConfig_FromConfig: what provideStorageBackend hands storagedb.New.
func TestStorageConfig_FromConfig(t *testing.T) {
	cloud := config.Default()
	cloud.Mode = config.ModeCloud
	cloud.Auth.Enabled = true
	cloud.Storage.Backend = config.StorageDatabase
	cloud.Storage.DatabaseURL = secret.New("postgres://baki@db.example.com/baki")
	cloud.Storage.DBMaxOpenConns = 12
	cloud.Storage.DBConnMaxLifetime = 0 // "0s": no age limit
	cloud.Storage.DBStatementTimeout = config.Duration(2 * time.Minute)
	cloud.Storage.MaxReplicas = 3
	cloud.Storage.DBServerMaxConns = 50
	cloud.Runtime.InstanceID = "baki-app--0000012-7d9c5b8f4-x2x7k"

	got := storageConfig(cloud)
	if !got.RequireSSL {
		t.Error("the cloud profile must require TLS by default")
	}
	if got.MaxOpenConns != 12 || got.ConnMaxLifetime != 0 || got.StatementTimeout != 2*time.Minute {
		t.Errorf("pool = %d conns, lifetime %v, statement timeout %v; want 12, 0, 2m", got.MaxOpenConns, got.ConnMaxLifetime, got.StatementTimeout)
	}
	if got.ExpectedReplicas != 3 || got.ServerMaxConns != 50 {
		t.Errorf("ExpectedReplicas %d, ServerMaxConns %d; want 3 and 50", got.ExpectedReplicas, got.ServerMaxConns)
	}
	if want := "baki-backend/baki-app--0000012-7d9c5b8f4-x2x7k"; got.ApplicationName != want {
		t.Errorf("ApplicationName = %q, want %q", got.ApplicationName, want)
	}

	localDB := config.Default()
	localDB.Storage.Backend = config.StorageDatabase
	if storageConfig(localDB).RequireSSL {
		t.Error("local+db must leave TLS optional by default")
	}
	cloud.Storage.DBRequireSSL = "false"
	if storageConfig(cloud).RequireSSL {
		t.Error("an explicit PAD_DB_REQUIRE_SSL=false must win")
	}
	localDB.Storage.DBRequireSSL = "yes"
	if !storageConfig(localDB).RequireSSL {
		t.Error("an explicit PAD_DB_REQUIRE_SSL=yes must win")
	}
}

// TestProvideStorageBackend_LeaserIsTheDatabases: with a database the graph holds the backend's lease taker, whose
// leases report a lost lock (Lost is a channel the heartbeat closes; the adapter over the plain lock has none), and
// the jobs get it: the audit backfill through fx (the scanner, the migration runner and the app's own jobs take it
// as a parameter, see their tests). Without a database (the desktop) there is none, and the jobs run alone.
func TestProvideStorageBackend_LeaserIsTheDatabases(t *testing.T) {
	cfg := serverStoreConfig(config.ModeCloud, true)
	cfg.Storage.DatabaseURL = secret.New(pgtest.NewDatabase(t, "")) // skips without DATABASE_URL
	cfg.Storage.DBRequireSSL = "false"
	var leaser lock.Leaser
	var backfill audit.BackfillDeps
	app := fxtest.New(t,
		fx.Supply(cfg),
		fx.Provide(provideStorageBackend),
		fx.Populate(&leaser),
		fx.Invoke(func(d audit.BackfillDeps) { backfill = d }),
	)
	app.RequireStart()
	defer app.RequireStop()

	if leaser == nil {
		t.Fatal("no lock.Leaser in a graph with a database")
	}
	if backfill.Leaser != leaser {
		t.Error("the audit backfill did not get the graph's leaser; it would fall back to the plain lock")
	}
	ctx := context.Background()
	const key int64 = 0x7465737461707031 // "testapp1", no job's
	lease, acquired, err := leaser.TryLease(ctx, key)
	if err != nil || !acquired {
		t.Fatalf("TryLease = %v, %v; want the free lock", acquired, err)
	}
	if lease.Lost() == nil {
		t.Error("the lease cannot report a lost lock (no heartbeat): the plain lock behind the adapter")
	}
	if _, again, err := leaser.TryLease(ctx, key); again || err != nil {
		t.Errorf("a second TryLease of a held key = %v, %v; want held", again, err)
	}
	lease.Release()
	next, acquired, err := leaser.TryLease(ctx, key)
	if err != nil || !acquired {
		t.Fatalf("TryLease after Release = %v, %v; want the freed lock", acquired, err)
	}
	next.Release()

	var none lock.Leaser
	desktop := fxtest.New(t, fx.Supply(config.Default()), fx.Provide(provideStorageBackend), fx.Populate(&none))
	desktop.RequireStart()
	defer desktop.RequireStop()
	if none != nil {
		t.Errorf("the desktop graph has a leaser (%T); without a database the jobs run alone", none)
	}
}

// TestStorageConfig_SessionGuards: what storageConfig hands the backend for the two session guards. The idle-
// transaction timeout is the configuration's effective one (the request timeout plus config.DBIdleTxTimeoutMargin
// unless PAD_DB_IDLE_TX_TIMEOUT sets it, "0s" turning it off). The cloud profile refuses a connection role that
// bypasses row-level security unless PAD_DB_ALLOW_BYPASSRLS is set; the other profiles only warn.
func TestStorageConfig_SessionGuards(t *testing.T) {
	cloud := serverStoreConfig(config.ModeCloud, true)
	got := storageConfig(cloud)
	if want := 30*time.Second + config.DBIdleTxTimeoutMargin; got.IdleInTxTimeout != want {
		t.Errorf("IdleInTxTimeout = %v, want the request timeout plus the margin, %v", got.IdleInTxTimeout, want)
	}
	if !got.RefuseRLSBypass {
		t.Error("the cloud profile must refuse a role that bypasses row-level security")
	}
	cloud.Runtime.RequestTimeout = config.Duration(2 * time.Minute)
	if got, want := storageConfig(cloud).IdleInTxTimeout, 2*time.Minute+config.DBIdleTxTimeoutMargin; got != want {
		t.Errorf("IdleInTxTimeout = %v after raising the request timeout, want %v", got, want)
	}
	cloud.Storage.DBIdleTxTimeout = config.ExplicitDuration(0)
	if got := storageConfig(cloud).IdleInTxTimeout; got != 0 {
		t.Errorf(`IdleInTxTimeout = %v with PAD_DB_IDLE_TX_TIMEOUT="0s", want 0 (off)`, got)
	}
	cloud.Storage.DBAllowBypassRLS = true
	if storageConfig(cloud).RefuseRLSBypass {
		t.Error("PAD_DB_ALLOW_BYPASSRLS=true must turn the refusal into the warning")
	}
	for _, accounts := range []bool{false, true} {
		if storageConfig(serverStoreConfig(config.ModeLocal, accounts)).RefuseRLSBypass {
			t.Errorf("local with a database (accounts %v) refuses a bypassing role; only the cloud profile does", accounts)
		}
	}
}

// TestProvideStorageBackend_ServerSessionTimeouts: a cloud deployment's pooled sessions carry the idle-transaction
// timeout the configuration derives (the request timeout plus the margin) beside the statement and lock timeouts,
// so a transaction left idle by a handler that outran its deadline is ended by the server instead of pinning its
// connection, its locks and the vacuum horizon. The role is the test role (NOBYPASSRLS), which the cloud profile's
// role check lets through.
func TestProvideStorageBackend_ServerSessionTimeouts(t *testing.T) {
	cfg := serverStoreConfig(config.ModeCloud, true)
	cfg.Storage.DatabaseURL = secret.New(pgtest.NewDatabase(t, "")) // skips without DATABASE_URL
	cfg.Storage.DBRequireSSL = "false"
	var pool storagedb.SQLProvider
	app := fxtest.New(t, fx.Supply(cfg), fx.Provide(provideStorageBackend), fx.Populate(&pool))
	app.RequireStart()
	defer app.RequireStop()

	var idle, statement, lock string
	if err := pool.DB().QueryRowContext(context.Background(), `SELECT current_setting('idle_in_transaction_session_timeout'),
		current_setting('statement_timeout'), current_setting('lock_timeout')`).Scan(&idle, &statement, &lock); err != nil {
		t.Fatalf("read the session settings: %v", err)
	}
	if idle != "90s" || statement != "30s" || lock != "30s" {
		t.Errorf("idle_in_transaction_session_timeout %q, statement_timeout %q, lock_timeout %q; want 90s, 30s, 30s", idle, statement, lock)
	}
}
