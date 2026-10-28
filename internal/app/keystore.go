package app

import (
	"fmt"

	"go.uber.org/fx"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/platform/secret"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage"
	storagedb "pad-analyzer/internal/storage/database"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// keystoreKey picks the database keystore's encryption key: the dedicated PAD_ENCRYPTION_KEY, else an
// operator-provided auth secret (legacy fallback). A secret minted for this process never qualifies: provider
// keys encrypted with it would be unreadable after a restart.
func keystoreKey(a config.AuthConfig) secret.Value {
	if a.EncryptionKey.IsSet() {
		return a.EncryptionKey
	}
	if a.SecretEphemeral {
		return secret.Value{}
	}
	return a.Secret
}

// keystorePreviousKeys are the keys PAD_ENCRYPTION_KEY replaced (PAD_ENCRYPTION_KEY_PREVIOUS, newest first): they
// open what they sealed until the re-seal pass has moved it. Only beside a dedicated key, as config validation
// requires; never beside the auth-secret fallback.
func keystorePreviousKeys(a config.AuthConfig) [][]byte {
	if !a.EncryptionKey.IsSet() || len(a.EncryptionKeyPrevious) == 0 {
		return nil
	}
	out := make([][]byte, len(a.EncryptionKeyPrevious))
	for i, k := range a.EncryptionKeyPrevious {
		out[i] = []byte(k.Reveal())
	}
	return out
}

// KeyStoreResult is the provider-key keystore and what only the encrypted database keystore offers beyond it, each an
// fx value that is nil with the OS keyring: the organisations' shared credentials, the listing of a user's key
// families and the re-seal pass of a key rotation. KeyReads and OrgKeyReads are the stores' request-scoped reads
// (R-provider-19: a scope's keys in one read, under the request's context): the keyring's own, the database
// keystore's both, OrgKeyReads nil with the keyring.
type KeyStoreResult struct {
	fx.Out
	Keys        service.KeyStore
	KeyReads    service.KeyReads
	OrgKeys     service.OrgKeyStore
	OrgKeyReads service.OrgKeyReads
	KeyFamilies storageif.SecretFamilyLister
	KeyReseal   storagedb.KeyResealer
}

// provideKeyStore builds the provider-API-key secret store: an encrypted,
// database-backed keystore with a database (preferring a dedicated
// PAD_ENCRYPTION_KEY over PAD_AUTH_SECRET so rotating the JWT secret doesn't
// brick stored provider keys), falling back to the OS-keyring default
// everywhere else. A plain fx provider (not an Invoke mutating a global), so
// consumers get the same DAG-resolved value and failures surface as provider
// errors.
func provideKeyStore(cfg *config.Config, keyStores storagedb.KeyStoreProvider) (KeyStoreResult, error) {
	kr := storage.NewKeyringStore()
	keyring := KeyStoreResult{Keys: kr, KeyReads: kr}
	if keyStores == nil {
		return keyring, nil
	}
	encKey := keystoreKey(cfg.Auth)
	switch {
	case !encKey.IsSet() && cfg.Auth.SecretEphemeral:
		logger.Warn("database storage without PAD_ENCRYPTION_KEY or PAD_AUTH_SECRET: provider keys stay in the OS keyring, " +
			"because a per-process secret would make them unreadable after a restart. Set PAD_ENCRYPTION_KEY to store them encrypted in the database.")
		return keyring, nil
	case !encKey.IsSet():
		return keyring, nil
	case !cfg.Auth.EncryptionKey.IsSet():
		// In the cloud the fallback is deprecated with a date (R-security-7): the warning says exactly what to set.
		// It is not refused yet, because a deployment on it has its stored values sealed under the auth secret.
		if msg := config.EncryptionKeyFallbackDeprecation(cfg); msg != "" {
			logger.Warn(msg, "removal", config.EncryptionKeyFallbackRemoval)
			break
		}
		logger.Warn("PAD_ENCRYPTION_KEY unset — falling back to PAD_AUTH_SECRET for keystore. " +
			"Set a dedicated PAD_ENCRYPTION_KEY so rotating the JWT secret doesn't brick stored provider keys.")
	}
	previous := keystorePreviousKeys(cfg.Auth)
	ks, err := keyStores.NewEncryptedKeyStore([]byte(encKey.Reveal()), previous...)
	if err != nil {
		return KeyStoreResult{}, fmt.Errorf("failed to init encrypted keystore: %w", err)
	}
	logger.Info("encrypted database keystore enabled", "previous_keys", len(previous))
	return KeyStoreResult{Keys: ks, KeyReads: ks, OrgKeys: ks, OrgKeyReads: ks, KeyFamilies: ks, KeyReseal: ks}, nil
}

// initFieldEncryption encrypts secret columns at rest (org notification channel URLs and signing secrets) with the
// keystore's key (opening with the keys a rotation replaced; scheduleSecretReseal moves those values). It runs before
// the server starts, so no request of this process writes plaintext; scheduleFieldEncryptionBackfill seals the rows
// written in plaintext before. Desktop mode has no server store.
func initFieldEncryption(cfg *config.Config, fe storagedb.FieldEncryptor) error {
	if fe == nil {
		return nil
	}
	key := keystoreKey(cfg.Auth)
	if !key.IsSet() {
		logger.Warn("no PAD_ENCRYPTION_KEY or PAD_AUTH_SECRET: org notification channel URLs and secrets are stored unencrypted")
		return nil
	}
	if err := fe.EnableFieldEncryption([]byte(key.Reveal()), keystorePreviousKeys(cfg.Auth)...); err != nil {
		return fmt.Errorf("field encryption: %w", err)
	}
	return nil
}
