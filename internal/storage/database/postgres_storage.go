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

	return b, nil
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
	return b.db.Close()
}

// DB exposes the underlying *sql.DB for callers that need pool-level
// telemetry (e.g. the metrics package's ObservePostgresPool). Not part
// of the StorageBackend interface — only the concrete Postgres backend
// has a pool to observe.
func (b *PostgresStorageBackend) DB() *sql.DB { return b.db }
