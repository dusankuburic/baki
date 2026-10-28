package storage

import (
	"errors"

	"pad-analyzer/internal/storage/interfaces"
)

// ErrSecretStorageUnavailable is returned by write operations when the active
// secret backend cannot persist secrets (e.g. a headless Linux/Docker
// deployment with no D-Bus Secret Service and no database-backed keystore
// configured). Provider keys are then simply not persisted.
var ErrSecretStorageUnavailable = errors.New("secret storage unavailable in this deployment")

// ErrSecretNotFound is returned by Get when no secret is stored for the
// requested provider.
var ErrSecretNotFound = errors.New("secret not found")

// ErrSecretDecryptFailed is returned by Get when a secret row exists but the
// ciphertext cannot be decrypted with the current deployment key — almost
// always because PAD_AUTH_SECRET was rotated. Previously this was masked
// as ErrSecretNotFound, so operators saw "no key configured" and re-entered
// the key, papering over the real cause indefinitely. Callers should
// surface a clear "your AI key needs to be re-entered" message to the user
// (or just delete the unreadable row and prompt).
var ErrSecretDecryptFailed = errors.New("secret could not be decrypted (deployment key may have rotated)")

// SecretStore abstracts provider-key persistence. The default implementation is
// the OS keychain (desktop mode, via NewKeyringSecretStore); cloud deployments
// construct an encrypted, database-backed store instead (see
// database.KeyStoreProvider) and inject it through fx like any other
// dependency — there is deliberately no package-level active-store global here
// (a prior SetSecretStore/CurrentSecretStore global made cloud-mode secret
// storage depend on fx *invoke* ordering instead of the DAG).
//
// All methods take a scope that namespaces the secret to an owner. An empty
// scope ("") is the legacy/local (single-user desktop) namespace and MUST map
// to the historical unscoped storage location so existing secrets keep working.
// In cloud mode the scope is the caller's user id, isolating per-user keys.
type SecretStore interface {
	Save(scope, provider, key string) error
	Get(scope, provider string) (string, error)
	Has(scope, provider string) (bool, error)
	Delete(scope, provider string) error
}

// NewKeyringSecretStore returns the default, OS-keychain-backed SecretStore
// (desktop/local mode). It degrades gracefully — never panics — when no
// keychain is reachable (headless/CI environments); see keyring.go.
func NewKeyringSecretStore() SecretStore { return keyringStore{} }

// KeyringStore is the OS-keychain SecretStore with its request-scoped reads (R-provider-19): the deployment provides
// both from one value, so no consumer type-asserts the store for them.
type KeyringStore interface {
	SecretStore
	interfaces.SecretStateLister
	interfaces.SecretContextReader
}

// NewKeyringStore is NewKeyringSecretStore with the request-scoped reads.
func NewKeyringStore() KeyringStore { return keyringStore{} }
