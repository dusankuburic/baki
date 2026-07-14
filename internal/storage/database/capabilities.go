package database

import (
	"context"
	"database/sql"
)

// This file names the Postgres-only capabilities that callers outside this
// package occasionally need beyond the storageif.StorageBackend role
// interfaces (main.go and the RLS middleware are the only two). *
// PostgresStorageBackend already implements each of these methods, so no
// implementation changes are needed — only the call sites' type assertions
// change, from the concrete *PostgresStorageBackend to the narrow interface
// that names what's actually being used. That keeps the coupling to "this
// needs a Postgres-backed store" visible and self-documenting at the
// assertion site, rather than hidden behind an opaque concrete-type check.

// SQLProvider is implemented by storage backends that expose their underlying
// *sql.DB, for callers that need to build a companion store sharing the same
// connection pool (e.g. a token store or blacklist keyed off the same DB).
type SQLProvider interface {
	DB() *sql.DB
}

// KeyStoreProvider is implemented by storage backends that can construct an
// encrypted, database-backed secret keystore.
type KeyStoreProvider interface {
	NewEncryptedKeyStore(secret []byte) (*EncryptedKeyStore, error)
}

// RLSBeginner is implemented by storage backends that support Postgres
// row-level-security transactions scoped to a caller.
type RLSBeginner interface {
	BeginRLS(ctx context.Context, userID string) (*sql.Tx, error)
}
