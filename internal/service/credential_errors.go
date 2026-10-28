package service

import (
	"errors"
	"fmt"
	"strings"
	"unicode"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/platform/apperr"
	"pad-analyzer/internal/storage"
)

// Codes a credential route answers (R-config-3, R-provider-2, R-provider-18).
const (
	// ProviderCodeKeyInvalid: /api/keys/save refused the key itself (empty, too long, a control or invisible
	// character).
	ProviderCodeKeyInvalid = "KEY_INVALID"
	// ProviderCodeKeyUnreadable: a stored credential exists but the keystore cannot decrypt it (the server's
	// encryption key changed); only entering it again repairs it.
	ProviderCodeKeyUnreadable = "KEY_UNREADABLE"
)

// MaxAPIKeyBytes bounds a stored API key; every vendor's key is far shorter. Mirrored by API_KEY_MAX_LENGTH in
// frontend/src/lib/constraints.ts.
const MaxAPIKeyBytes = 8 << 10

// checkAPIKey returns key trimmed, or a KEY_INVALID refusal: an empty key would read as "configured" in the
// catalogue (the keystore row exists) while every turn finds no key, and a control or invisible character is
// never part of a key (a paste that carried a line break, an escape sequence or a zero-width space).
func checkAPIKey(key string) (string, error) {
	key = strings.TrimSpace(key)
	switch {
	case key == "":
		return "", apperr.Invalid(ProviderCodeKeyInvalid, "the key is empty")
	case len(key) > MaxAPIKeyBytes:
		return "", apperr.Invalid(ProviderCodeKeyInvalid, fmt.Sprintf("the key is %d bytes, at most %d", len(key), MaxAPIKeyBytes))
	case strings.IndexFunc(key, invisibleInKey) >= 0:
		return "", apperr.Invalid(ProviderCodeKeyInvalid, "the key contains a control or invisible character (a line break, a tab or a zero-width space, for example)")
	}
	return key, nil
}

// invisibleInKey reports a rune no key holds: a control character (Cc) or an invisible format character (Cf: a
// zero-width space, a byte-order mark), which a paste carries along and the vendor then refuses on every request.
func invisibleInKey(r rune) bool {
	return unicode.IsControl(r) || unicode.Is(unicode.Cf, r)
}

// What a credential route answers for ErrSecretDecryptFailed: the caller's own credential, and an organisation's.
const (
	keyUnreadableHTTPMsg       = "the stored credential could not be read (the server's encryption key may have changed) — enter it again"
	keyUnreadableSharedHTTPMsg = "your organisation's shared credential could not be read (the server's encryption key may have changed) — ask your organisation admin to enter it again"
)

// SecretStoreError is err as the API answers it when a secret store failure is in its chain, wherever it
// surfaced: an unreachable store is 503 SECRET_STORAGE_UNAVAILABLE (the Azure routes' code for it) and a
// credential the keystore cannot decrypt 422 KEY_UNREADABLE. Any other err, and an *apperr.Error that already
// decided its answer, is returned unchanged. render.Error applies it, so every route shares the mapping.
func SecretStoreError(err error) error {
	if _, ok := apperr.As(err); ok {
		return err
	}
	switch {
	case errors.Is(err, storage.ErrSecretStorageUnavailable):
		return apperr.Unavailable(ProviderCodeSecretStorageUnavailable, "secret storage is unavailable").Wrap(err)
	case errors.Is(err, storage.ErrSecretDecryptFailed):
		if orgErr := (*ai.OrgKeyError)(nil); errors.As(err, &orgErr) {
			return apperr.Unprocessable(ProviderCodeKeyUnreadable, keyUnreadableSharedHTTPMsg).
				WithDetails(map[string]any{"shared": true}).Wrap(err)
		}
		return apperr.Unprocessable(ProviderCodeKeyUnreadable, keyUnreadableHTTPMsg).Wrap(err)
	}
	return err
}
