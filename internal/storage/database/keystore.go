package database

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"
	"time"

	"pad-analyzer/internal/storage"
	"pad-core/logger"
)

const keystoreTimeout = 5 * time.Second

// EncryptedKeyStore persists provider API keys in PostgreSQL, encrypted at rest
// with AES-256-GCM. It implements storage.SecretStore so it can be injected as
// the active secret backend in cloud mode, where the OS keychain is absent.
//
// The encryption key should be a dedicated deployment secret (PAD_ENCRYPTION_KEY),
// separate from the JWT signing key (PAD_AUTH_SECRET). For backward compatibility
// the auth secret is still accepted when no dedicated key is configured, but
// rotating either then no longer affects the other. Rotating the encryption key
// renders previously stored ciphertext undecryptable (Get reports the key as not
// found, and the provider must be re-authenticated).
type EncryptedKeyStore struct {
	db   *sql.DB
	aead cipher.AEAD
}

// NewEncryptedKeyStore builds a database-backed encrypted keystore. The secret
// must be non-empty; the AES-256 key is its SHA-256 digest.
func (b *PostgresStorageBackend) NewEncryptedKeyStore(secret []byte) (*EncryptedKeyStore, error) {
	aead, err := newKeystoreAEAD(secret)
	if err != nil {
		return nil, err
	}
	return &EncryptedKeyStore{db: b.db, aead: aead}, nil
}

// newKeystoreAEAD derives an AES-256-GCM AEAD from a deployment secret.
func newKeystoreAEAD(secret []byte) (cipher.AEAD, error) {
	if len(secret) == 0 {
		return nil, errors.New("keystore: empty encryption secret")
	}
	sum := sha256.Sum256(secret)
	block, err := aes.NewCipher(sum[:])
	if err != nil {
		return nil, fmt.Errorf("keystore: new cipher: %w", err)
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, fmt.Errorf("keystore: new gcm: %w", err)
	}
	return aead, nil
}

// keyAAD returns the AES-GCM associated data that binds a ciphertext to its
// (scope, provider) row. Authentication fails if a stored ciphertext is moved to
// a different identity, preventing a row-swap confused-deputy by anyone able to
// write the provider_keys table.
func keyAAD(scope, provider string) []byte {
	return []byte(scope + "\x00" + provider)
}

// encrypt seals plaintext with a fresh random nonce and binds it to aad,
// returning a base64(nonce||ciphertext) string suitable for text-column storage.
func (s *EncryptedKeyStore) encrypt(plaintext string, aad []byte) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("keystore: nonce: %w", err)
	}
	sealed := s.aead.Seal(nonce, nonce, []byte(plaintext), aad)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// decrypt reverses encrypt; aad must match the value passed to encrypt.
func (s *EncryptedKeyStore) decrypt(encoded string, aad []byte) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("keystore: decode: %w", err)
	}
	ns := s.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("keystore: ciphertext too short")
	}
	nonce, ct := raw[:ns], raw[ns:]
	plaintext, err := s.aead.Open(nil, nonce, ct, aad)
	if err != nil {
		return "", fmt.Errorf("keystore: open: %w", err)
	}
	return string(plaintext), nil
}

// Save upserts the encrypted key for a (scope, provider) pair. An empty scope
// is the legacy/local namespace (stored as user_id = ”).
func (s *EncryptedKeyStore) Save(scope, provider, key string) error {
	ct, err := s.encrypt(key, keyAAD(scope, provider))
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(context.Background(), keystoreTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO provider_keys (user_id, provider, ciphertext, updated_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (user_id, provider) DO UPDATE SET ciphertext = EXCLUDED.ciphertext, updated_at = NOW()`,
		scope, provider, ct,
	); err != nil {
		return fmt.Errorf("keystore: save: %w", err)
	}
	return nil
}

// Get returns the decrypted key, storage.ErrSecretNotFound if no row exists,
// or storage.ErrSecretDecryptFailed if a row exists but cannot be decrypted
// with the current deployment key (almost always: PAD_AUTH_SECRET was
// rotated). Distinguishing the two lets the API surface a clear "your AI
// key needs to be re-entered after secret rotation" rather than masking the
// rotation as "no key configured" and prompting the user to enter it again
// indefinitely.
func (s *EncryptedKeyStore) Get(scope, provider string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), keystoreTimeout)
	defer cancel()
	var ct string
	err := s.db.QueryRowContext(ctx, `SELECT ciphertext FROM provider_keys WHERE user_id = $1 AND provider = $2`, scope, provider).Scan(&ct)
	if errors.Is(err, sql.ErrNoRows) {
		return "", storage.ErrSecretNotFound
	}
	if err != nil {
		return "", fmt.Errorf("keystore: get: %w", err)
	}
	plaintext, err := s.decrypt(ct, keyAAD(scope, provider))
	if err != nil {
		// Log so the operator can see that a rotation invalidated stored
		// keys; don't log scope (user id) at info level — keep it at warn
		// with provider only.
		logger.Warn("keystore: ciphertext decrypt failed (deployment key may have rotated)",
			"provider", provider, "error", err)
		return "", storage.ErrSecretDecryptFailed
	}
	return plaintext, nil
}

// Has reports whether a key row exists for the (scope, provider) pair.
func (s *EncryptedKeyStore) Has(scope, provider string) (bool, error) {
	ctx, cancel := context.WithTimeout(context.Background(), keystoreTimeout)
	defer cancel()
	var exists bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS (SELECT 1 FROM provider_keys WHERE user_id = $1 AND provider = $2)`, scope, provider,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("keystore: has: %w", err)
	}
	return exists, nil
}

// Delete removes a stored provider key. Deleting a missing key is a no-op.
func (s *EncryptedKeyStore) Delete(scope, provider string) error {
	ctx, cancel := context.WithTimeout(context.Background(), keystoreTimeout)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM provider_keys WHERE user_id = $1 AND provider = $2`, scope, provider); err != nil {
		return fmt.Errorf("keystore: delete: %w", err)
	}
	return nil
}
