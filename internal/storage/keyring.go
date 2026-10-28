package storage

import (
	"context"
	"errors"
	"sync"

	"pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"

	"github.com/zalando/go-keyring"
)

const keyringService = "pad-analyzer"

// keyringStore is the default SecretStore, backed by the OS keychain (right
// for desktop mode). In headless/cloud deployments the keychain is typically
// unavailable, so a database-backed store is injected instead.
type keyringStore struct{}

var warnUnavailableOnce sync.Once

// The provider catalogue and listing read keys under the request's context and by scope (R-provider-19). The OS
// keychain takes no context and cannot enumerate its entries: a read checks the context before it starts, and a
// scope's states are its keys read one by one, as before.
var (
	_ interfaces.SecretContextReader = keyringStore{}
	_ interfaces.SecretStateLister   = keyringStore{}
)

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

// keyringEntry returns the keychain entry name for a (scope, provider) pair:
// an empty scope keeps the historical "apikey:<provider>" name so existing
// desktop entries resolve unchanged; non-empty namespaces per owner.
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
// keychain backend is unavailable — an unavailable backend behaves as "no key
// configured" instead of surfacing infrastructure errors.
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

// GetCtx is Get once ctx is still live (the keychain call itself cannot be cancelled).
func (s keyringStore) GetCtx(ctx context.Context, scope, provider string) (string, error) {
	if err := ctx.Err(); err != nil {
		return "", err
	}
	return s.Get(scope, provider)
}

// HasCtx is Has once ctx is still live.
func (s keyringStore) HasCtx(ctx context.Context, scope, provider string) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	return s.Has(scope, provider)
}

// KeyStates reads each of keys in turn, stopping when ctx ends; an absent key (or an unavailable keychain) has
// no entry, an empty value is SecretEmpty. The keychain holds no ciphertext, so no key is unreadable.
func (s keyringStore) KeyStates(ctx context.Context, scope string, keys []string) (map[string]interfaces.SecretState, error) {
	out := make(map[string]interfaces.SecretState, len(keys))
	for _, key := range keys {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		v, err := keyring.Get(keyringService, keyringEntry(scope, key))
		switch {
		case err != nil:
			if keyringUnavailable(err) {
				warnUnavailable("get", err)
			}
		case v == "":
			out[key] = interfaces.SecretEmpty
		default:
			out[key] = interfaces.SecretUsable
		}
	}
	return out, nil
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
	// Backend unavailable: the key may still be there, so a removal that did not happen is not reported as done
	// (the settings form would show the provider as not configured while the key stays in the keychain).
	warnUnavailable("delete", err)
	return ErrSecretStorageUnavailable
}
