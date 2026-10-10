// Package database implements StorageBackend backed by PostgreSQL, intended for cloud/multi-tenant
// deployments; local desktop mode uses the filesystem backend instead.
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"strconv"
	"sync/atomic"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/storage/azblob"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/uptrace/opentelemetry-go-extra/otelsql"
	semconv "go.opentelemetry.io/otel/semconv/v1.26.0"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/storage/settingsdoc"
)

// pgErrUniqueViolation is the SQLSTATE for a unique constraint violation.
const pgErrUniqueViolation = "23505"

// pgErrSerializationFailure is the SQLSTATE returned to a transaction that
// could not be serialized; the caller should retry.
const pgErrSerializationFailure = "40001"

// isPgErrCode reports whether err is a *pgconn.PgError with the given SQLSTATE.
func isPgErrCode(err error, code string) bool {
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == code
	}
	return false
}

// PostgresStorageBackend implements interfaces.StorageBackend using PostgreSQL.
type PostgresStorageBackend struct {
	db *sql.DB

	blobClient *azblob.Client
	container  string
	// cleaner runs deferred blob-cleanup work on a bounded worker pool; nil when blob storage is not
	// configured or the backend was built directly (some tests) — scheduleBlobCleanup then detaches.
	cleaner *blobCleaner

	// embeddingDim is the configured vector dimension for knowledge-base embeddings; chunks whose
	// width differs are kept (JSONB) but excluded from the pgvector index. 0 ⇒ 1536.
	embeddingDim int
	// hasPgvector is true when the `vector` extension AND knowledge_chunks.embedding_vec exist —
	// SearchKnowledge can then push similarity ordering into the database. Detected once after migrate.
	hasPgvector bool
	// hnswIterativeScan is true when pgvector knows hnsw.iterative_scan (0.8+): the HNSW scan can
	// then keep walking until the org filter has let LIMIT rows through. Detected once after migrate.
	hnswIterativeScan bool

	// azureExportConfig supplies the data-subject export's Azure resource
	// configuration (SetAzureExportConfigSource); nil until DI installs it.
	azureExportConfig atomic.Pointer[interfaces.AzureExportConfigFunc]

	// sealer encrypts secret columns at rest (EnableFieldEncryption); nil = stored as given.
	sealer atomic.Pointer[fieldSealer]

	// settingsSections declares the settings document's sections; nil = settingsdoc.Default (tests swap in their own).
	settingsSections *settingsdoc.Registry
}

// Config holds the connection settings for the PostgreSQL backend.
type Config struct {
	DSN          string
	MaxOpenConns int
	// MaxIdleConns caps the idle connections kept for reuse; <= 0 (the default) means "as many as MaxOpenConns" — see poolLimits.
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
	// StatementTimeout is applied to every pooled connection as Postgres statement_timeout AND lock_timeout,
	// cancelling a runaway query or lock wait server-side. 0 disables; migrations lift it for their session.
	StatementTimeout time.Duration
	// RequireSSL, when true, refuses a DSN whose sslmode does not enforce an encrypted connection — a
	// fail-fast guard so a misconfigured cloud deployment carrying credentials over plaintext is caught at boot.
	RequireSSL bool

	AzureStorageAccount   string
	AzureStorageContainer string
	// AzureBlobConnectionString, when set, builds the blob client from a
	// connection string (emulator / non-MI) instead of account + Managed Identity.
	AzureBlobConnectionString string

	// EmbeddingDim is the configured knowledge-base embedding dimension. 0 ⇒
	// default 1536; mismatched chunks are excluded from the pgvector index.
	EmbeddingDim int

	// ExpectedReplicas is how many replicas share the server (PAD_MAX_REPLICAS); 0 counts as one. Only the boot
	// check of MaxOpenConns against the server's max_connections reads it.
	ExpectedReplicas int
	// ServerMaxConns is the server's max_connections as the deployment knows it (PAD_DB_SERVER_MAX_CONNS); 0 =
	// unknown. The boot check uses it when the server's own settings cannot be read (a pooler in front, a
	// restricted role); the server's answer wins when there is one.
	ServerMaxConns int
	// ApplicationName is the application_name every connection reports, so pg_stat_activity and pg_locks tell the
	// replicas (and the job locks they hold) apart, e.g. "baki-backend/<instance>". Empty means
	// DefaultApplicationName; an application_name in the DSN (or PGAPPNAME) wins.
	ApplicationName string
}

// Fixed boot-check log messages, for alerts and runbooks (docs/architecture/operations.md); never reworded.
const (
	// LogPoolOverServerLimit (WARN): MaxOpenConns × the expected replicas is more than the server accepts.
	LogPoolOverServerLimit = "postgres: connection pools can exceed max_connections"
	// LogPgBouncerPort (WARN): the DSN points at port 6432, where Azure Flexible Server's PgBouncer listens.
	LogPgBouncerPort = "PgBouncer transaction mode is unsupported: session advisory locks"
)

// DefaultApplicationName is the application_name of a connection when neither the DSN nor Config names one.
const DefaultApplicationName = "baki-backend"

// pgBouncerPort is the port of Azure Flexible Server's built-in PgBouncer (transaction pooling by default).
const pgBouncerPort = 6432

func DefaultConfig(dsn string) Config {
	return Config{
		DSN:          dsn,
		MaxOpenConns: 25,
		// 0 = match MaxOpenConns (poolLimits), so an operator raising
		// PAD_DB_MAX_OPEN_CONNS does not reintroduce idle churn.
		MaxIdleConns:    0,
		ConnMaxLifetime: time.Hour,
		ConnMaxIdleTime: 5 * time.Minute,
		// Bounded by default: an unbounded query is the outage nobody can see.
		StatementTimeout: 30 * time.Second,
	}
}

// New opens a PostgreSQL connection, configures the pool, and runs migrations.
// With Managed Identity a fresh Entra token is injected per connection by
// azureMIConnector (no background refresh goroutine).
func New(ctx context.Context, cfg Config) (*PostgresStorageBackend, error) {
	pgxCfg, err := pgx.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}

	// Fail fast on an insecure connection when TLS is required — checked after parsing (a malformed DSN
	// surfaces its real error) and before any network/blob setup.
	if cfg.RequireSSL && !sslModeIsSecure(cfg.DSN) {
		return nil, fmt.Errorf("postgres: TLS required but DSN sslmode=%q is insecure; use sslmode=require or verify-full", sslModeFromDSN(cfg.DSN))
	}
	// Before the first connection: through a transaction-mode pooler that one may already fail (its startup
	// parameters), and this line names the cause.
	warnIfPgBouncerPort(slog.Default(), pgxCfg)

	b := &PostgresStorageBackend{}

	// Initialize Azure Blob Storage if configured: a container plus an auth source (account name →
	// Managed Identity, the prod default; or a connection string → emulator / non-MI).
	if cfg.AzureStorageContainer != "" && (cfg.AzureStorageAccount != "" || cfg.AzureBlobConnectionString != "") {
		// Explicit retry bounds so a slow/unavailable blob backend can't extend a request unboundedly:
		// MaxRetryDelay caps backoff, TryTimeout bounds each attempt.
		blobOpts := &azblob.ClientOptions{
			ClientOptions: azcore.ClientOptions{
				Retry: policy.RetryOptions{
					MaxRetries:    4,
					RetryDelay:    1 * time.Second,
					MaxRetryDelay: 30 * time.Second,
					TryTimeout:    30 * time.Second,
				},
			},
		}
		var client *azblob.Client
		if cfg.AzureBlobConnectionString != "" {
			c, err := azblob.NewClientFromConnectionString(cfg.AzureBlobConnectionString, blobOpts)
			if err != nil {
				return nil, fmt.Errorf("azure: failed to create blob client from connection string: %w", err)
			}
			client = c
		} else {
			cred, err := azidentity.NewDefaultAzureCredential(nil)
			if err != nil {
				return nil, fmt.Errorf("azure: failed to obtain credential for blob storage: %w", err)
			}
			serviceURL := fmt.Sprintf("https://%s.blob.core.windows.net/", cfg.AzureStorageAccount)
			c, err := azblob.NewClient(serviceURL, cred, blobOpts)
			if err != nil {
				return nil, fmt.Errorf("azure: failed to create blob client: %w", err)
			}
			client = c
		}
		b.blobClient = client
		b.container = cfg.AzureStorageContainer
		b.cleaner = newBlobCleaner()
		// Probe the container so misconfiguration surfaces in logs immediately, but do NOT fail startup:
		// the readiness probe (CheckBlob) keeps the pod out of rotation until blob is reachable.
		checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err = client.ServiceClient().NewContainerClient(cfg.AzureStorageContainer).GetProperties(checkCtx, nil)
		cancel()
		if err != nil {
			slog.Warn("azure: blob container not reachable at startup (check name, credentials, and the Storage Blob Data Contributor role); readiness will gate traffic until it recovers",
				"container", cfg.AzureStorageContainer, "error", err)
		}
		slog.Info("azure: blob storage enabled", "account", cfg.AzureStorageAccount, "container", cfg.AzureStorageContainer)
	}

	if pgxCfg.RuntimeParams == nil {
		pgxCfg.RuntimeParams = map[string]string{}
	}
	// Server-side statement and lock budgets on every connection. No
	// idle_in_transaction_session_timeout, though no longer for a chat stream
	// idling in the request transaction: the streaming routes run outside it
	// (rls_middleware.go), and it ends before the first response byte (ADR
	// 0004). It still idles while its handler works between statements (an
	// outbound call, CPU), and lives until the handler returns, not until the
	// deadline (rls.go begins it WithoutCancel): a timeout would have to outlast
	// the slowest handler.
	if cfg.StatementTimeout > 0 {
		ms := strconv.FormatInt(cfg.StatementTimeout.Milliseconds(), 10)
		pgxCfg.RuntimeParams["statement_timeout"] = ms
		pgxCfg.RuntimeParams["lock_timeout"] = ms
	}
	setApplicationName(pgxCfg.RuntimeParams, cfg.ApplicationName)

	otelOpts := []otelsql.Option{
		otelsql.WithAttributes(semconv.DBSystemPostgreSQL),
		otelsql.WithDBName(pgxCfg.Database),
	}

	var db *sql.DB
	if pgxCfg.Password == "managed-identity" {
		// Managed Identity: a fresh Entra token is injected per connection by the connector (no shared
		// mutable password field, so no data race). Validate once up front to fail fast on bad RBAC.
		provider, err := newAzureTokenProvider()
		if err != nil {
			return nil, fmt.Errorf("azure: create credential: %w", err)
		}
		if _, err := provider.GetAccessToken(ctx); err != nil {
			return nil, fmt.Errorf("azure: initial token fetch: %w", err)
		}
		db = otelsql.OpenDB(newAzureMIConnector(provider, pgxCfg), otelOpts...)
	} else {
		connStr := stdlib.RegisterConnConfig(pgxCfg)
		var err error
		db, err = otelsql.Open("pgx", connStr, otelOpts...)
		if err != nil {
			return nil, fmt.Errorf("open pgx with otel: %w", err)
		}
	}

	maxOpen, maxIdle := poolLimits(cfg)
	db.SetMaxOpenConns(maxOpen)
	db.SetMaxIdleConns(maxIdle)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	idleTime := cfg.ConnMaxIdleTime
	if idleTime == 0 {
		idleTime = 5 * time.Minute
	}
	db.SetConnMaxIdleTime(idleTime)

	b.db = db
	if err := b.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	// Apply the embedding-dimension contract (default config.DefaultEmbeddingDim,
	// the width the AI factory asks embedding models for).
	b.embeddingDim = cfg.EmbeddingDim
	if b.embeddingDim <= 0 {
		b.embeddingDim = config.DefaultEmbeddingDim
	}

	// Detect whether pgvector is available so SearchKnowledge can push the
	// similarity ordering into the database; without it the Go-side ranker
	// keeps local mode and extension-less deployments working.
	b.hasPgvector = detectPgvector(ctx, db)
	if b.hasPgvector {
		b.hnswIterativeScan = detectHNSWIterativeScan(ctx, db)
	}

	// Surface a silently-disabled security layer: a superuser/BYPASSRLS role makes
	// every RLS policy inert, and nothing else reveals it — queries succeed, tests
	// pass, the schema still lists the policies.
	warnIfRLSBypassed(ctx, db)

	// Every replica's pool full at once (a burst, a stuck dependency) must fit the server, or the next connection
	// fails with "too many clients".
	warnIfPoolExceedsServer(ctx, slog.Default(), func(ctx context.Context) (serverConnLimits, error) {
		return readServerConnLimits(ctx, db)
	}, maxOpen, cfg.ExpectedReplicas, cfg.ServerMaxConns)

	return b, nil
}

// serverConnLimits are the server's connection settings: max_connections, of which superuser_reserved_connections
// are for superusers only and reserved_connections (PostgreSQL 16) for pg_use_reserved_connections members.
type serverConnLimits struct {
	Max, SuperuserReserved, Reserved int
}

// available is what an ordinary role such as the application's can open.
func (l serverConnLimits) available() int { return l.Max - l.SuperuserReserved - l.Reserved }

// readServerConnLimits reads the settings in one round trip (what SHOW reports); reserved_connections is 0 before
// PostgreSQL 16, which does not know it.
func readServerConnLimits(ctx context.Context, db *sql.DB) (serverConnLimits, error) {
	var l serverConnLimits
	err := db.QueryRowContext(ctx, `SELECT current_setting('max_connections')::int,
		current_setting('superuser_reserved_connections')::int,
		COALESCE(current_setting('reserved_connections', true), '0')::int`,
	).Scan(&l.Max, &l.SuperuserReserved, &l.Reserved)
	return l, err
}

// warnIfPoolExceedsServer logs LogPoolOverServerLimit when maxOpen × max(replicas, 1) is more than the server
// accepts. It warns and never fails the boot: the replica count is deployment knowledge, and what else uses the
// server (an operator's session, another app) the process cannot know. During a rolling deploy one more replica
// runs beside the others, so the suggested size leaves room for it, as infra/main.bicep's derivation does. When the
// server's settings cannot be read, configuredMax (PAD_DB_SERVER_MAX_CONNS) stands in, with no reserves known.
func warnIfPoolExceedsServer(ctx context.Context, log *slog.Logger, read func(context.Context) (serverConnLimits, error), maxOpen, replicas, configuredMax int) {
	if maxOpen <= 0 {
		return // no cap: there is no pool size to compare
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	from := "server"
	lim, err := read(ctx)
	switch {
	case err == nil:
	case configuredMax > 0:
		log.Debug("postgres: connection limits not read; checking the pool against PAD_DB_SERVER_MAX_CONNS", "err", err)
		lim, from = serverConnLimits{Max: configuredMax}, "PAD_DB_SERVER_MAX_CONNS"
	default:
		log.Debug("postgres: connection limits not read; pool check skipped", "err", err)
		return
	}
	replicas = max(replicas, 1)
	total, available := maxOpen*replicas, lim.available()
	if total <= available {
		return
	}
	log.Warn(LogPoolOverServerLimit,
		"max_open_conns", maxOpen,
		"replicas", replicas,
		"pools_total", total,
		"server_max_connections", lim.Max,
		"server_reserved_connections", lim.SuperuserReserved+lim.Reserved,
		"available", available,
		"limits_from", from,
		"suggested_max_open_conns", max(1, available/(replicas+1)),
		"remediation", "lower PAD_DB_MAX_OPEN_CONNS, or set dbServerMaxConnections in infra/main.bicep to derive it (docs/architecture/operations.md)")
}

// warnIfPgBouncerPort logs LogPgBouncerPort when a host of the DSN uses PgBouncer's port. In transaction mode a
// session advisory lock and its unlock run on different server sessions, so every Singleton job's lease is lost
// (ADR 0023), and the startup statement_timeout may be refused. Read from the parsed config, so a multi-host DSN is
// checked host by host and the password is never near the log.
func warnIfPgBouncerPort(log *slog.Logger, cfg *pgx.ConnConfig) {
	type hostPort struct {
		host string
		port uint16
	}
	hosts := []hostPort{{cfg.Host, cfg.Port}}
	for _, f := range cfg.Fallbacks {
		hosts = append(hosts, hostPort{f.Host, f.Port})
	}
	for _, h := range hosts {
		if h.port == pgBouncerPort {
			log.Warn(LogPgBouncerPort, "host", h.host, "port", h.port,
				"remediation", "connect to the server's own port (5432), or run PgBouncer in session mode (docs/architecture/operations.md)")
			return
		}
	}
}

// setApplicationName names the connections, unless the DSN (or PGAPPNAME) already does.
func setApplicationName(params map[string]string, name string) {
	if params["application_name"] != "" {
		return
	}
	if name == "" {
		name = DefaultApplicationName
	}
	params["application_name"] = name
}

// rlsBypassedByRole reports whether the connection's current role ignores Row-Level Security (a superuser
// or BYPASSRLS). The third return is false when the question could not be answered, so callers stay quiet.
func rlsBypassedByRole(ctx context.Context, db *sql.DB) (role string, bypassed bool, known bool) {
	err := db.QueryRowContext(ctx,
		`SELECT current_user, rolsuper OR rolbypassrls FROM pg_roles WHERE rolname = current_user`,
	).Scan(&role, &bypassed)
	if err != nil {
		return "", false, false
	}
	return role, bypassed, true
}

// warnIfRLSBypassed logs once at startup when the configured database role
// defeats Row-Level Security. WARNS rather than refusing to boot: the
// docker-compose default connects as `postgres`, so failing closed would
// brick existing deployments on upgrade.
func warnIfRLSBypassed(ctx context.Context, db *sql.DB) {
	role, bypassed, known := rlsBypassedByRole(ctx, db)
	if !known || !bypassed {
		return
	}
	slog.Warn("postgres: connection role bypasses Row-Level Security — the RLS policies in this schema are INERT",
		"role", role,
		"impact", "tenant isolation rests entirely on the application authz layer; the database-side backstop is not in effect",
		"remediation", "connect as a role created with NOSUPERUSER NOBYPASSRLS that owns the application database")
}

// poolLimits resolves the pool's open/idle caps. The idle cap defaults to the
// open cap: with 25 open but only 5 idle, every burst above five queries
// closed and re-dialled the surplus (TLS + auth, an Entra token under MI) — a
// load simulation opened 505 connections where 15 sufficed. Idle connections
// are still reaped after ConnMaxIdleTime; an explicit MaxIdleConns still wins.
func poolLimits(cfg Config) (maxOpen, maxIdle int) {
	maxOpen, maxIdle = cfg.MaxOpenConns, cfg.MaxIdleConns
	if maxIdle <= 0 && maxOpen > 0 {
		maxIdle = maxOpen
	}
	return maxOpen, maxIdle
}

func (b *PostgresStorageBackend) Ping(ctx context.Context) error {
	return b.db.PingContext(ctx)
}

// CheckBlob verifies the configured blob container is reachable; nil when blob
// storage is not configured, so a readiness probe can call it unconditionally.
func (b *PostgresStorageBackend) CheckBlob(ctx context.Context) error {
	if b.blobClient == nil {
		return nil
	}
	_, err := b.blobClient.ServiceClient().NewContainerClient(b.container).GetProperties(ctx, nil)
	return err
}

func (b *PostgresStorageBackend) Close() error {
	// Drain in-flight blob cleanups before dropping the pool so a graceful
	// shutdown doesn't abandon the last window of superseded blobs.
	if b.cleaner != nil {
		b.cleaner.Stop()
	}
	return b.db.Close()
}

// DB exposes the underlying *sql.DB for pool-level telemetry (e.g.
// metrics.ObservePostgresPool) — only this backend has a pool to observe.
func (b *PostgresStorageBackend) DB() *sql.DB { return b.db }

// HasPgvector reports whether server-side vector similarity search is active
// (vector extension + embedding_vec column); exposed for observability and
// tests — SearchKnowledge falls back to the in-Go ranker when false.
func (b *PostgresStorageBackend) HasPgvector() bool { return b.hasPgvector }

// EmbeddingDim returns the configured knowledge-base embedding dimension.
func (b *PostgresStorageBackend) EmbeddingDim() int { return b.embeddingDim }

// detectPgvector reports whether the `vector` extension AND
// knowledge_chunks.embedding_vec exist — the prerequisites for pushing
// similarity search into the database; absence is the normal local-mode
// case, logged at info.
func detectPgvector(ctx context.Context, db *sql.DB) bool {
	var ok bool
	// pg_vector is a query that only succeeds when both the extension is loaded
	// and the typed column is present; checking both in one round-trip.
	err := db.QueryRowContext(ctx, `
		SELECT EXISTS (SELECT 1 FROM pg_extension WHERE extname = 'vector')
		 AND EXISTS (
			SELECT 1 FROM information_schema.columns
			WHERE table_name = 'knowledge_chunks' AND column_name = 'embedding_vec'
		 )`).Scan(&ok)
	if err != nil {
		slog.Info("pgvector not detected; knowledge search stays Go-side", "error", err)
		return false
	}
	if ok {
		slog.Info("pgvector detected; knowledge search uses server-side similarity")
	} else {
		slog.Info("pgvector not detected; knowledge search stays Go-side")
	}
	return ok
}
