package database

import (
	"context"
	"database/sql"

	"pad-analyzer/internal/platform/reqtx"
	"pad-analyzer/internal/platform/txhook"
	"pad-analyzer/internal/storage/interfaces"
)

// What only the database offers, beyond the StorageBackend role interfaces. main.go's provideStorageBackend
// provides each as an fx value taken from the concrete backend, nil without a database, so no caller type-asserts
// a storage value to find out what it can do (internal/archtest pins the assertions left). The cross-replica lock
// is lock.GlobalLocker and the request transaction reqtx.Opener (RequestTx).

// SQLProvider exposes the backend's pool, for a companion store on the same pool (the token blacklist).
type SQLProvider interface {
	DB() *sql.DB
}

// KeyStoreProvider constructs the encrypted, database-backed secret keystore.
type KeyStoreProvider interface {
	NewEncryptedKeyStore(secret []byte) (*EncryptedKeyStore, error)
}

// FieldEncryptor encrypts secret columns at rest (field_crypto.go) and seals the rows written before.
type FieldEncryptor interface {
	EnableFieldEncryption(secret []byte) error
	EncryptOrgChannelsAtRest(ctx context.Context) (int, error)
}

// RLSBeginner starts Postgres row-level-security transactions scoped to a caller.
type RLSBeginner interface {
	BeginRLS(ctx context.Context, userID string) (*sql.Tx, error)
}

// AzureRegistryProvider holds the Azure AI Foundry model registry tables (migration v25) and the data-subject
// export's hook for the personal resources' public configuration. DI uses the registry only with auth enabled
// (the tables' owners are real users and organisations); otherwise the azure-models.json file store serves.
type AzureRegistryProvider interface {
	AzureRegistry() interfaces.AzureRegistryStore
	SetAzureExportConfigSource(fn interfaces.AzureExportConfigFunc)
}

// RequestTx is the request transaction the RLS middleware opens on b: a lazy scope per request (WithLazyRLS) and
// a fresh scope per post-commit hook (RLSHookRunner).
func RequestTx(b RLSBeginner) reqtx.Opener {
	return requestTx{b: b}
}

type requestTx struct{ b RLSBeginner }

func (r requestTx) Open(ctx context.Context, userID string, failOpen bool) (context.Context, reqtx.Scope) {
	ctx, s := WithLazyRLS(ctx, r.b, userID, failOpen)
	return ctx, requestScope{s}
}

func (r requestTx) HookRunner(base context.Context, userID string) txhook.Runner {
	return RLSHookRunner(base, r.b, userID)
}

// requestScope adds Begin, the scope's EnsureRLS, to RLSScope.
type requestScope struct{ *RLSScope }

func (s requestScope) Begin() error {
	_, _, err := s.get()
	return err
}
