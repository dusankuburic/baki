package database

import (
	"context"
	"database/sql"

	"pad-analyzer/internal/storage/interfaces"
)

// Postgres-only capabilities that callers outside this package occasionally
// need beyond the StorageBackend role interfaces (main.go and the RLS
// middleware are the only two). Callers type-assert to the narrow interface
// that names what is actually used, keeping the "needs a Postgres-backed
// store" coupling visible at the assertion site.

// SQLProvider is implemented by storage backends that expose their underlying
// *sql.DB, for callers building a companion store on the same pool (e.g. the
// blacklist).
type SQLProvider interface {
	DB() *sql.DB
}

// KeyStoreProvider is implemented by storage backends that can construct an
// encrypted, database-backed secret keystore.
type KeyStoreProvider interface {
	NewEncryptedKeyStore(secret []byte) (*EncryptedKeyStore, error)
}

// FieldEncryptor is implemented by storage backends that can encrypt secret
// columns at rest (field_crypto.go) and seal the rows written before.
type FieldEncryptor interface {
	EnableFieldEncryption(secret []byte) error
	EncryptOrgChannelsAtRest(ctx context.Context) (int, error)
}

// RLSBeginner is implemented by storage backends that support Postgres
// row-level-security transactions scoped to a caller.
type RLSBeginner interface {
	BeginRLS(ctx context.Context, userID string) (*sql.Tx, error)
}

// AzureRegistryProvider is implemented by storage backends that hold the
// Azure AI Foundry model registry tables (migration v25). DI uses it only with
// auth enabled (the tables' owners are real users/orgs); otherwise the
// azure-models.json file store serves.
type AzureRegistryProvider interface {
	AzureRegistry() interfaces.AzureRegistryStore
}
