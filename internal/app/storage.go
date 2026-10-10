package app

import (
	"context"
	"fmt"
	"strings"
	"time"

	"go.uber.org/fx"

	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/config"
	padmetrics "pad-analyzer/internal/metrics"
	"pad-analyzer/internal/platform/lock"
	"pad-analyzer/internal/platform/reqtx"
	"pad-analyzer/internal/service"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// StorageResult is the storage backend and what only the database offers beyond its role interfaces, each an fx
// value of its own that is nil without a database (the desktop): consumers take the one they need instead of
// type-asserting the backend.
type StorageResult struct {
	fx.Out
	Backend       storageif.StorageBackend
	Locker        lock.GlobalLocker
	Leaser        lock.Leaser // the jobs' lock (lifecycle.Singleton): the same advisory locks, with a heartbeat
	Pool          storagedb.SQLProvider
	KeyStores     storagedb.KeyStoreProvider
	FieldCrypto   storagedb.FieldEncryptor
	AuditOrgs     audit.OrgBackfiller
	Blob          service.BlobHealthChecker
	AzureRegistry storagedb.AzureRegistryProvider
	// RequestTx and Idempotency need an authenticated caller to scope to, so they are nil with auth off too.
	RequestTx   reqtx.Opener
	Idempotency storageif.IdempotencyStore
}

// vectorIndexBuildTimeout bounds the background HNSW build of the knowledge
// index (EnsureVectorIndex); a corpus that takes longer is left for the next
// boot to finish (the invalid leftover is dropped).
const vectorIndexBuildTimeout = 2 * time.Hour

func provideStorageBackend(lc fx.Lifecycle, cfg *config.Config) (StorageResult, error) {
	if cfg.Storage.Backend != config.StorageDatabase {
		return StorageResult{Backend: nil}, nil
	}

	dbCfg := storageConfig(cfg)
	pgBackend, err := storagedb.New(context.Background(), dbCfg)
	if err != nil {
		return StorageResult{}, fmt.Errorf("failed to connect to database: %w", err)
	}

	poolStop := make(chan struct{})
	// The knowledge HNSW index is built in the background after boot (CREATE
	// INDEX CONCURRENTLY cannot run inside the migration transaction, and a
	// large corpus takes minutes): detached from the start hook's context,
	// bounded by its own timeout, cancelled on stop. An interrupted build
	// leaves an invalid index the next boot drops and redoes.
	indexCtx, cancelIndexBuild := context.WithTimeout(context.Background(), vectorIndexBuildTimeout)
	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			go func() {
				t := time.NewTicker(15 * time.Second)
				defer t.Stop()
				for {
					select {
					case <-poolStop:
						return
					case <-t.C:
						padmetrics.ObservePostgresPool(pgBackend.DB())
					}
				}
			}()
			go func() {
				defer cancelIndexBuild()
				if err := pgBackend.EnsureVectorIndex(indexCtx, pgBackend.EmbeddingDim()); err != nil {
					logger.Warn("knowledge vector index build did not finish", "error", err)
				}
			}()
			return nil
		},
		OnStop: func(ctx context.Context) error {
			cancelIndexBuild()
			close(poolStop)
			return pgBackend.Close()
		},
	})

	res := StorageResult{
		Backend:       pgBackend,
		Locker:        pgBackend,
		Leaser:        pgBackend,
		Pool:          pgBackend,
		KeyStores:     pgBackend,
		FieldCrypto:   pgBackend,
		AuditOrgs:     pgBackend,
		Blob:          pgBackend,
		AzureRegistry: pgBackend,
	}
	if capability.Of(cfg).Has(capability.RowLevelSecurity) {
		res.RequestTx = storagedb.RequestTx(pgBackend)
		res.Idempotency = pgBackend
	}
	return res, nil
}

// storageConfig is the Postgres backend's configuration: its defaults with the operator's PAD_DB_* settings over
// them, durations included (parsed and range-checked by config.Load).
func storageConfig(cfg *config.Config) storagedb.Config {
	dbCfg := storagedb.DefaultConfig(cfg.Storage.DatabaseURL.Reveal())
	// Resolve the TLS requirement: explicit PAD_DB_REQUIRE_SSL wins; otherwise
	// the cloud profile requires TLS by default (credentials over plaintext is
	// a security bug), local leaves it optional for bundled/dev Postgres.
	switch strings.ToLower(cfg.Storage.DBRequireSSL) {
	case "true", "1", "yes":
		dbCfg.RequireSSL = true
	case "false", "0", "no":
		dbCfg.RequireSSL = false
	default:
		dbCfg.RequireSSL = capability.Of(cfg).Profile() == capability.Cloud
	}
	if cfg.Storage.DBMaxOpenConns > 0 {
		dbCfg.MaxOpenConns = cfg.Storage.DBMaxOpenConns
	}
	if cfg.Storage.DBMaxIdleConns > 0 {
		dbCfg.MaxIdleConns = cfg.Storage.DBMaxIdleConns
	}
	// config.Default holds the backend's defaults for these, so 0 here is an explicit "0s": no age limit, the
	// backend's idle default, no statement timeout.
	dbCfg.ConnMaxLifetime = cfg.Storage.DBConnMaxLifetime.Std()
	dbCfg.ConnMaxIdleTime = cfg.Storage.DBConnMaxIdleTime.Std()
	dbCfg.StatementTimeout = cfg.Storage.DBStatementTimeout.Std()

	// Apply Azure Blob Storage config: enabled when a container is set
	// together with an auth source — an account name (Managed Identity) or a
	// connection string (emulator / non-MI).
	if cfg.Storage.AzureStorageContainer != "" &&
		(cfg.Storage.AzureStorageAccount != "" || cfg.Storage.AzureBlobConnectionString.IsSet()) {
		dbCfg.AzureStorageAccount = cfg.Storage.AzureStorageAccount
		dbCfg.AzureStorageContainer = cfg.Storage.AzureStorageContainer
		dbCfg.AzureBlobConnectionString = cfg.Storage.AzureBlobConnectionString.Reveal()
	}
	// Knowledge-base embedding dimension; gates which chunks are
	// pgvector-searchable (default 1536 applied in the storage layer when 0).
	dbCfg.EmbeddingDim = cfg.Storage.EmbeddingDim

	// The pool's boot check against the server's max_connections (docs/architecture/operations.md).
	dbCfg.ExpectedReplicas = cfg.Storage.MaxReplicas
	dbCfg.ServerMaxConns = cfg.Storage.DBServerMaxConns
	// pg_stat_activity and pg_locks name the replica holding a connection (a job's lock, say).
	if cfg.Runtime.InstanceID != "" {
		dbCfg.ApplicationName = storagedb.DefaultApplicationName + "/" + cfg.Runtime.InstanceID
	}
	return dbCfg
}
