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
	"sync"
	"time"

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

// azureRefreshState holds the credential and pgx config needed to keep the
// Managed Identity token alive for connections opened after each refresh cycle.
// mu protects pgxCfg.Password from being read by a connection open while the
// refresh goroutine is rewriting it.
type azureRefreshState struct {
	mu       sync.Mutex
	provider *azureTokenProvider
	pgxCfg   *pgx.ConnConfig
}

// PostgresStorageBackend implements interfaces.StorageBackend using PostgreSQL.
type PostgresStorageBackend struct {
	db           *sql.DB
	azureRefresh *azureRefreshState // non-nil when using Azure Managed Identity
}

// Config holds the connection settings for the PostgreSQL backend.
type Config struct {
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
}

// DefaultConfig returns conservative connection-pool defaults.
func DefaultConfig(dsn string) Config {
	return Config{
		DSN:             dsn,
		MaxOpenConns:    25,
		MaxIdleConns:    5,
		ConnMaxLifetime: time.Hour,
	}
}

// New opens a PostgreSQL connection, configures the pool, and runs migrations.
// ctx governs the application lifetime: it is used for migrations and, when
// Azure Managed Identity is configured, drives the background token-refresh
// goroutine (cancelled on SIGTERM / graceful shutdown).
func New(ctx context.Context, cfg Config) (*PostgresStorageBackend, error) {
	pgxCfg, err := pgx.ParseConfig(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse dsn: %w", err)
	}

	b := &PostgresStorageBackend{}

	connStr := stdlib.RegisterConnConfig(pgxCfg)

	if pgxCfg.Password == "managed-identity" {
		provider, err := newAzureTokenProvider()
		if err != nil {
			return nil, fmt.Errorf("azure: create credential: %w", err)
		}
		token, err := provider.GetToken(ctx)
		if err != nil {
			return nil, fmt.Errorf("azure: initial token fetch: %w", err)
		}
		pgxCfg.Password = token
		// Re-register so the pool's connStr picks up the resolved token.
		stdlib.UnregisterConnConfig(connStr)
		connStr = stdlib.RegisterConnConfig(pgxCfg)
		b.azureRefresh = &azureRefreshState{provider: provider, pgxCfg: pgxCfg}
	}

	db, err := otelsql.Open("pgx", connStr,
		otelsql.WithAttributes(semconv.DBSystemPostgreSQL),
		otelsql.WithDBName(pgxCfg.Database),
	)
	if err != nil {
		return nil, fmt.Errorf("open pgx with otel: %w", err)
	}

	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)
	// Reclaim idle connections after 5 minutes so the pool shrinks during
	// quiet periods. Without this, every replica holds MaxIdleConns sockets
	// open forever, multiplying the server-side connection count under
	// horizontal scaling.
	db.SetConnMaxIdleTime(5 * time.Minute)

	b.db = db
	if err := b.migrate(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}

	if b.azureRefresh != nil {
		go b.runAzureTokenRefresh(ctx)
	}

	return b, nil
}

// runAzureTokenRefresh refreshes the Managed Identity token every 20 minutes so
// that new connections opened after each cycle always get a valid token.
// Azure tokens expire after ~24 hours; refreshing every 20 minutes gives ample
// margin.  Existing idle connections are cycled out naturally by ConnMaxLifetime
// (default 1 hour), so they too will pick up fresh tokens before expiry.
//
// The pgxCfg.Password field is mutated under azureRefresh.mu because pgx may
// read it concurrently when opening new pooled connections.
func (b *PostgresStorageBackend) runAzureTokenRefresh(ctx context.Context) {
	ticker := time.NewTicker(20 * time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			token, err := b.azureRefresh.provider.GetToken(ctx)
			if err != nil {
				slog.Error("azure: managed identity token refresh failed", "err", err)
				continue
			}
			b.azureRefresh.mu.Lock()
			b.azureRefresh.pgxCfg.Password = token
			b.azureRefresh.mu.Unlock()
			slog.Info("azure: managed identity token refreshed")
		}
	}
}

func (b *PostgresStorageBackend) Ping(ctx context.Context) error {
	return b.db.PingContext(ctx)
}

func (b *PostgresStorageBackend) Close() error {
	return b.db.Close()
}

// DB exposes the underlying *sql.DB for callers that need pool-level
// telemetry (e.g. the metrics package's ObservePostgresPool). Not part
// of the StorageBackend interface — only the concrete Postgres backend
// has a pool to observe.
func (b *PostgresStorageBackend) DB() *sql.DB { return b.db }
