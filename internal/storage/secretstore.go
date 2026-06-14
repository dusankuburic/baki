package storage

import (
	"errors"
	"sync"
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
// the OS keychain (desktop mode); cloud deployments inject an encrypted,
// database-backed store via SetSecretStore.
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

var (
	storeMu     sync.RWMutex
	activeStore SecretStore = keyringStore{}
)

// SetSecretStore replaces the active secret backend. Call once at startup
// (e.g. to switch to a database-backed encrypted keystore in cloud mode).
func SetSecretStore(s SecretStore) {
	storeMu.Lock()
	defer storeMu.Unlock()
	activeStore = s
}

// CurrentSecretStore returns the active secret backend. Used by the DI
// container to inject the SecretStore into services.
func CurrentSecretStore() SecretStore {
	storeMu.RLock()
	defer storeMu.RUnlock()
	return activeStore
}

func currentStore() SecretStore {
	storeMu.RLock()
	defer storeMu.RUnlock()
	return activeStore
}

// SaveApiKeyScoped persists a provider API key for the given scope (owner).
// An empty scope uses the legacy/local unscoped namespace.
func SaveApiKeyScoped(scope, provider, key string) error {
	return currentStore().Save(scope, provider, key)
}

// GetApiKeyScoped returns the stored key for a provider within scope, or
// ErrSecretNotFound if absent (or the backend is unavailable).
func GetApiKeyScoped(scope, provider string) (string, error) {
	return currentStore().Get(scope, provider)
}

// HasApiKeyScoped reports whether a key is configured for the provider in scope.
func HasApiKeyScoped(scope, provider string) (bool, error) {
	return currentStore().Has(scope, provider)
}

// DeleteApiKeyScoped removes a stored provider key within scope.
func DeleteApiKeyScoped(scope, provider string) error {
	return currentStore().Delete(scope, provider)
}

// SaveApiKey persists a provider API key in the legacy/local (unscoped) namespace.
// Retained for callers without a user identity (e.g. desktop OAuth device flows).
func SaveApiKey(provider, key string) error { return SaveApiKeyScoped("", provider, key) }

// GetApiKey returns the stored key for a provider in the legacy/local namespace.
func GetApiKey(provider string) (string, error) { return GetApiKeyScoped("", provider) }

// HasApiKey reports whether a key is configured in the legacy/local namespace.
func HasApiKey(provider string) (bool, error) { return HasApiKeyScoped("", provider) }

// DeleteApiKey removes a stored provider key in the legacy/local namespace.
func DeleteApiKey(provider string) error { return DeleteApiKeyScoped("", provider) }
