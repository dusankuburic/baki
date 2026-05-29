package storage

import (
	"errors"
	"sync"

	"pad-analyzer/internal/logger"

	"github.com/zalando/go-keyring"
)

const keyringService = "pad-analyzer"

// keyringStore is the default SecretStore, backed by the OS keychain.
// It is the right choice for desktop (Tauri) mode. In headless/cloud
// deployments the keychain is typically unavailable, so a database-backed
// store should be injected via SetSecretStore instead.
type keyringStore struct{}

var warnUnavailableOnce sync.Once

// keyringUnavailable reports whether err indicates the OS secret service is
// unreachable (as opposed to the key simply not being present).
func keyringUnavailable(err error) bool {
	return err != nil && !errors.Is(err, keyring.ErrNotFound)
}

func warnUnavailable(op string, err error) {
	warnUnavailableOnce.Do(func() {
		logger.Warn("OS keychain unavailable; provider key storage is disabled in this deployment",
			"op", op, "error", err)
	})
}

// keyringEntry returns the keychain entry name for a (scope, provider) pair.
// An empty scope yields the historical "apikey:<provider>" name so existing
// desktop entries continue to resolve unchanged; a non-empty scope namespaces
// the entry per owner.
func keyringEntry(scope, provider string) string {
	if scope == "" {
		return "apikey:" + provider
	}
	return "apikey:" + scope + ":" + provider
}

func (keyringStore) Save(scope, provider, key string) error {
	if err := keyring.Set(keyringService, keyringEntry(scope, provider), key); err != nil {
		warnUnavailable("save", err)
		return ErrSecretStorageUnavailable
	}
	return nil
}

// Get returns the stored key, or ErrSecretNotFound if it is absent OR the
// keychain backend is unavailable. Treating an unavailable backend as
// "not found" lets headless deployments behave as "no key configured" instead
// of surfacing infrastructure errors to callers.
func (keyringStore) Get(scope, provider string) (string, error) {
	v, err := keyring.Get(keyringService, keyringEntry(scope, provider))
	if err != nil {
		if keyringUnavailable(err) {
			warnUnavailable("get", err)
		}
		return "", ErrSecretNotFound
	}
	return v, nil
}

func (keyringStore) Has(scope, provider string) (bool, error) {
	_, err := keyring.Get(keyringService, keyringEntry(scope, provider))
	if err != nil {
		if keyringUnavailable(err) {
			warnUnavailable("has", err)
		}
		// Absent key or unavailable backend → no key from the caller's view.
		return false, nil
	}
	return true, nil
}

func (keyringStore) Delete(scope, provider string) error {
	err := keyring.Delete(keyringService, keyringEntry(scope, provider))
	if err == nil || errors.Is(err, keyring.ErrNotFound) {
		return nil
	}
	// Backend unavailable: nothing to delete, don't surface as an error.
	warnUnavailable("delete", err)
	return nil
}
