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

// SecretStore abstracts provider-key persistence. The default implementation is
// the OS keychain (desktop mode); cloud deployments inject an encrypted,
// database-backed store via SetSecretStore.
type SecretStore interface {
	Save(provider, key string) error
	Get(provider string) (string, error)
	Has(provider string) (bool, error)
	Delete(provider string) error
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

func currentStore() SecretStore {
	storeMu.RLock()
	defer storeMu.RUnlock()
	return activeStore
}

// SaveApiKey persists a provider API key in the active secret backend.
func SaveApiKey(provider, key string) error {
	return currentStore().Save(provider, key)
}

// GetApiKey returns the stored key for a provider, or ErrSecretNotFound if it
// is absent (or the backend is unavailable, which callers treat the same way).
func GetApiKey(provider string) (string, error) {
	return currentStore().Get(provider)
}

// HasApiKey reports whether a key is configured for the provider.
func HasApiKey(provider string) (bool, error) {
	return currentStore().Has(provider)
}

// DeleteApiKey removes a stored provider key.
func DeleteApiKey(provider string) error {
	return currentStore().Delete(provider)
}
