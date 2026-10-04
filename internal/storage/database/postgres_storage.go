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
}

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

	// Server-side statement and lock budgets on every connection. Deliberately NOT
	// idle_in_transaction_session_timeout: the RLS middleware holds a transaction
	// open through a chat stream that legitimately idles for minutes.
	if cfg.StatementTimeout > 0 {
		if pgxCfg.RuntimeParams == nil {
			pgxCfg.RuntimeParams = map[string]string{}
		}
		ms := strconv.FormatInt(cfg.StatementTimeout.Milliseconds(), 10)
		pgxCfg.RuntimeParams["statement_timeout"] = ms
		pgxCfg.RuntimeParams["lock_timeout"] = ms
	}

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

	return b, nil
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
