package database

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"io"

	"pad-analyzer/internal/storage"
)

// EncryptedKeyStore persists provider API keys in PostgreSQL, encrypted at rest
// with AES-256-GCM. It implements storage.SecretStore so it can be injected as
// the active secret backend in cloud mode, where the OS keychain is absent.
//
// The encryption key is derived from a deployment secret (PAD_AUTH_SECRET) via
// SHA-256, so rotating that secret renders previously stored ciphertext
// undecryptable (Get then reports the key as not found, and the provider must
// be re-authenticated).
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

// encrypt seals plaintext with a fresh random nonce, returning a
// base64(nonce||ciphertext) string suitable for text-column storage.
func (s *EncryptedKeyStore) encrypt(plaintext string) (string, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return "", fmt.Errorf("keystore: nonce: %w", err)
	}
	sealed := s.aead.Seal(nonce, nonce, []byte(plaintext), nil)
	return base64.StdEncoding.EncodeToString(sealed), nil
}

// decrypt reverses encrypt.
func (s *EncryptedKeyStore) decrypt(encoded string) (string, error) {
	raw, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("keystore: decode: %w", err)
	}
	ns := s.aead.NonceSize()
	if len(raw) < ns {
		return "", errors.New("keystore: ciphertext too short")
	}
	nonce, ct := raw[:ns], raw[ns:]
	plaintext, err := s.aead.Open(nil, nonce, ct, nil)
	if err != nil {
		return "", fmt.Errorf("keystore: open: %w", err)
	}
	return string(plaintext), nil
}

// Save upserts the encrypted key for a (scope, provider) pair. An empty scope
// is the legacy/local namespace (stored as user_id = '').
func (s *EncryptedKeyStore) Save(scope, provider, key string) error {
	ct, err := s.encrypt(key)
	if err != nil {
		return err
	}
	if _, err := s.db.Exec(
		`INSERT INTO provider_keys (user_id, provider, ciphertext, updated_at)
		 VALUES ($1, $2, $3, NOW())
		 ON CONFLICT (user_id, provider) DO UPDATE SET ciphertext = EXCLUDED.ciphertext, updated_at = NOW()`,
		scope, provider, ct,
	); err != nil {
		return fmt.Errorf("keystore: save: %w", err)
	}
	return nil
}

// Get returns the decrypted key, or storage.ErrSecretNotFound if no row exists
// or the stored ciphertext can no longer be decrypted (e.g. the deployment
// secret was rotated).
func (s *EncryptedKeyStore) Get(scope, provider string) (string, error) {
	var ct string
	err := s.db.QueryRow(`SELECT ciphertext FROM provider_keys WHERE user_id = $1 AND provider = $2`, scope, provider).Scan(&ct)
	if errors.Is(err, sql.ErrNoRows) {
		return "", storage.ErrSecretNotFound
	}
	if err != nil {
		return "", fmt.Errorf("keystore: get: %w", err)
	}
	plaintext, err := s.decrypt(ct)
	if err != nil {
		// Undecryptable ciphertext is treated as "not configured" so the caller
		// can prompt for re-authentication rather than fail hard.
		return "", storage.ErrSecretNotFound
	}
	return plaintext, nil
}

// Has reports whether a key row exists for the (scope, provider) pair.
func (s *EncryptedKeyStore) Has(scope, provider string) (bool, error) {
	var exists bool
	err := s.db.QueryRow(
		`SELECT EXISTS (SELECT 1 FROM provider_keys WHERE user_id = $1 AND provider = $2)`, scope, provider,
	).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("keystore: has: %w", err)
	}
	return exists, nil
}

// Delete removes a stored provider key. Deleting a missing key is a no-op.
func (s *EncryptedKeyStore) Delete(scope, provider string) error {
	if _, err := s.db.Exec(`DELETE FROM provider_keys WHERE user_id = $1 AND provider = $2`, scope, provider); err != nil {
		return fmt.Errorf("keystore: delete: %w", err)
	}
	return nil
}
