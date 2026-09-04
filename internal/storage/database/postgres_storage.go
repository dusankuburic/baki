// Package database implements StorageBackend backed by PostgreSQL.
// It is intended for cloud/multi-tenant deployments.  Local desktop mode uses
// the filesystem backend instead.
package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
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
	// cleaner runs deferred blob-cleanup work on a bounded worker pool. Non-nil
	// only when blob storage is configured (created in New). Backends built
	// directly (some tests) leave it nil; scheduleBlobCleanup falls back to a
	// detached goroutine in that case.
	cleaner *blobCleaner

	// embeddingDim is the configured vector dimension for knowledge-base
	// embeddings. Chunks whose embedding width differs are kept (JSONB) but
	// excluded from the pgvector index so similarity search stays well-defined.
	// 0 ⇒ 1536 (the constructor applies the default).
	embeddingDim int
	// hasPgvector is true when the `vector` extension is installed AND the
	// knowledge_chunks.embedding_vec column exists — i.e. SearchKnowledge can
	// push similarity ordering into the database. Detected once after migrate.
	hasPgvector bool
}

// Config holds the connection settings for the PostgreSQL backend.
type Config struct {
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	ConnMaxIdleTime time.Duration
	// RequireSSL, when true, refuses to open a DSN whose sslmode does not
	// enforce an encrypted connection (sslmode=disable/allow/prefer or unset).
	// This is a fail-fast guard so a misconfigured cloud deployment carrying
	// credentials over plaintext is caught at boot rather than silently allowed.
	RequireSSL bool

	AzureStorageAccount   string
	AzureStorageContainer string
	// AzureBlobConnectionString, when set, builds the blob client from a
	// connection string (emulator / non-MI) instead of account + Managed Identity.
	AzureBlobConnectionString string

	// EmbeddingDim is the configured knowledge-base embedding dimension. 0 ⇒
	// default 1536 (OpenAI text-embedding-3-small). Chunks whose embedding width
	// differs are excluded from the pgvector index (see PostgresStorageBackend).
	EmbeddingDim int
}

func DefaultConfig(dsn string) Config {
	return Config{
		DSN:             dsn,
		MaxOpenConns:    25,
		MaxIdleConns:    5,
		ConnMaxLifetime: time.Hour,
		ConnMaxIdleTime: 5 * time.Minute,
	}
}

// New opens a PostgreSQL connection, configures the pool, and runs migrations.
// ctx is used for migrations and, when Azure Managed Identity is configured, for
// the initial token validation. With Managed Identity a fresh Entra token is
// injected per connection by azureMIConnector, so there is no background
// refresh goroutine to manage.
func New(ctx context.Context, cfg Config) (*PostgresStorageBackend, error) {
	pgxCfg, err := pgx.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}

	// Fail fast on an insecure connection when the operator (or cloud-mode
	// default) requires TLS. Checked after parsing so a malformed DSN still
	// surfaces its real error, and before any network/blob setup so it never
	// depends on a reachable server.
	if cfg.RequireSSL && !sslModeIsSecure(cfg.DSN) {
		return nil, fmt.Errorf("postgres: TLS required but DSN sslmode=%q is insecure; use sslmode=require or verify-full", sslModeFromDSN(cfg.DSN))
	}

	b := &PostgresStorageBackend{}

	// Initialize Azure Blob Storage if configured: a container plus an auth source
	// (account name → Managed Identity, the prod default; or a connection string
	// → emulator / non-MI).
	if cfg.AzureStorageContainer != "" && (cfg.AzureStorageAccount != "" || cfg.AzureBlobConnectionString != "") {
		// Explicit retry policy: the SDK retries on 429/5xx by default, but we set
		// bounds so a slow/unavailable blob backend can't extend a request
		// unboundedly. MaxRetryDelay caps backoff; TryTimeout bounds each attempt.
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
		// Probe the container so misconfiguration surfaces in logs immediately,
		// but do NOT fail startup: a transient blob outage during a pod restart
		// must not block boot (the DB may be perfectly healthy). The readiness
		// probe (CheckBlob) keeps the pod out of rotation until blob is reachable.
		checkCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err = client.ServiceClient().NewContainerClient(cfg.AzureStorageContainer).GetProperties(checkCtx, nil)
		cancel()
		if err != nil {
			slog.Warn("azure: blob container not reachable at startup (check name, credentials, and the Storage Blob Data Contributor role); readiness will gate traffic until it recovers",
				"container", cfg.AzureStorageContainer, "error", err)
		}
		slog.Info("azure: blob storage enabled", "account", cfg.AzureStorageAccount, "container", cfg.AzureStorageContainer)
	}

	otelOpts := []otelsql.Option{
		otelsql.WithAttributes(semconv.DBSystemPostgreSQL),
		otelsql.WithDBName(pgxCfg.Database),
	}

	var db *sql.DB
	if pgxCfg.Password == "managed-identity" {
		// Managed Identity: a fresh Entra token is injected per connection by the
		// connector (no shared mutable password field, so no data race with the
		// driver opening connections). Validate once up front to fail fast on bad
		// credentials / RBAC.
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

	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
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

	// Apply the embedding-dimension contract (default 1536). Chunks whose width
	// differs are kept but excluded from the pgvector index.
	b.embeddingDim = cfg.EmbeddingDim
	if b.embeddingDim <= 0 {
		b.embeddingDim = 1536
	}

	// Detect whether pgvector is available so SearchKnowledge can push the
	// similarity ordering into the database. Falls back to Go-side ranking
	// (rankKnowledgeChunks) when the extension or column is absent — local mode
	// and any deployment without the vector extension keep working unchanged.
	b.hasPgvector = detectPgvector(ctx, db)

	// Surface a silently-disabled security layer. Every RLS policy in this
	// schema is defense-in-depth behind the Go authz layer, but "defense in
	// depth" is only true if the depth exists — and a superuser/BYPASSRLS
	// connection role makes every policy inert no matter how they are written
	// (FORCE ROW LEVEL SECURITY does not apply to such roles either). Nothing
	// in the running system otherwise reveals this: queries succeed, tests
	// pass, and the schema still lists the policies.
	warnIfRLSBypassed(ctx, db)

	return b, nil
}

// rlsBypassedByRole reports whether the connection's current role ignores
// Row-Level Security — i.e. it is a superuser or carries BYPASSRLS. The second
// return is false when the question could not be answered (no connection, or
// the catalog is unreadable), so callers can stay quiet rather than guess.
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
// defeats Row-Level Security. It deliberately WARNS rather than refusing to
// boot: the shipped docker-compose default connects as `postgres`, so failing
// closed would brick existing deployments on upgrade. The operator gets the
// exact remediation instead.
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

func (b *PostgresStorageBackend) Ping(ctx context.Context) error {
	return b.db.PingContext(ctx)
}

// CheckBlob verifies that the configured blob container is reachable. It returns
// nil when blob storage is not configured (nothing to check), so it is safe to
// call unconditionally from a readiness probe.
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

// DB exposes the underlying *sql.DB for callers that need pool-level
// telemetry (e.g. the metrics package's ObservePostgresPool). Not part
// of the StorageBackend interface — only the concrete Postgres backend
// has a pool to observe.
func (b *PostgresStorageBackend) DB() *sql.DB { return b.db }

// HasPgvector reports whether server-side vector similarity search is active
// (the vector extension + embedding_vec column are present). Exposed for
// observability/health and integration tests; SearchKnowledge falls back to
// the in-Go ranker when this is false.
func (b *PostgresStorageBackend) HasPgvector() bool { return b.hasPgvector }

// EmbeddingDim returns the configured knowledge-base embedding dimension.
func (b *PostgresStorageBackend) EmbeddingDim() int { return b.embeddingDim }

// detectPgvector reports whether the `vector` extension is installed AND the
// knowledge_chunks.embedding_vec column exists — the two prerequisites for
// pushing similarity search into the database. A failure or absence returns
// false (logged at info, not warn: absence is the normal local-mode /
// no-extension case, not an error condition).
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
