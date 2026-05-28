package database

import (
	"errors"
	"os"
	"testing"

	"pad-analyzer/internal/storage"
)

// newTestStore builds an EncryptedKeyStore with no DB, for exercising the
// crypto layer (encrypt/decrypt) in isolation.
func newTestStore(t *testing.T, secret string) *EncryptedKeyStore {
	t.Helper()
	aead, err := newKeystoreAEAD([]byte(secret))
	if err != nil {
		t.Fatalf("newKeystoreAEAD: %v", err)
	}
	return &EncryptedKeyStore{aead: aead}
}

func TestEncryptedKeyStore_EncryptDecryptRoundTrip(t *testing.T) {
	s := newTestStore(t, "a-sufficiently-long-deployment-secret-123456")

	const plaintext = "sk-test-1234567890abcdef"
	ct, err := s.encrypt(plaintext)
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if ct == plaintext {
		t.Fatal("ciphertext must not equal plaintext")
	}

	got, err := s.decrypt(ct)
	if err != nil {
		t.Fatalf("decrypt: %v", err)
	}
	if got != plaintext {
		t.Fatalf("round-trip mismatch: got %q want %q", got, plaintext)
	}
}

func TestEncryptedKeyStore_NonceIsRandom(t *testing.T) {
	s := newTestStore(t, "a-sufficiently-long-deployment-secret-123456")

	a, err := s.encrypt("same-value")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	b, err := s.encrypt("same-value")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}
	if a == b {
		t.Fatal("two encryptions of the same plaintext must differ (random nonce)")
	}
}

func TestEncryptedKeyStore_WrongSecretFailsToDecrypt(t *testing.T) {
	enc := newTestStore(t, "the-original-deployment-secret-aaaaaaaaaaaa")
	ct, err := enc.encrypt("super-secret-token")
	if err != nil {
		t.Fatalf("encrypt: %v", err)
	}

	// A store built from a different secret must not be able to decrypt.
	other := newTestStore(t, "a-completely-different-secret-bbbbbbbbbbbb")
	if _, err := other.decrypt(ct); err == nil {
		t.Fatal("expected decrypt to fail with a different secret")
	}
}

func TestEncryptedKeyStore_EmptySecretRejected(t *testing.T) {
	if _, err := newKeystoreAEAD(nil); err == nil {
		t.Fatal("expected error for empty secret")
	}
}

// TestEncryptedKeyStore_DBRoundTrip exercises Save/Get/Has/Delete against a real
// Postgres instance. Skipped unless DATABASE_URL is set.
func TestEncryptedKeyStore_DBRoundTrip(t *testing.T) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		t.Skip("DATABASE_URL not set; skipping Postgres keystore integration test")
	}

	b, err := New(DefaultConfig(dsn))
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	defer b.Close()

	ks, err := b.NewEncryptedKeyStore([]byte("a-sufficiently-long-deployment-secret-123456"))
	if err != nil {
		t.Fatalf("NewEncryptedKeyStore: %v", err)
	}

	const provider = "test-provider-keystore"
	defer ks.Delete(provider)

	// Absent initially.
	if has, err := ks.Has(provider); err != nil || has {
		t.Fatalf("Has before save: has=%v err=%v", has, err)
	}
	if _, err := ks.Get(provider); !errors.Is(err, storage.ErrSecretNotFound) {
		t.Fatalf("Get before save: want ErrSecretNotFound, got %v", err)
	}

	// Save then read back.
	const key = "sk-abc-deadbeef-0123456789"
	if err := ks.Save(provider, key); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if has, err := ks.Has(provider); err != nil || !has {
		t.Fatalf("Has after save: has=%v err=%v", has, err)
	}
	got, err := ks.Get(provider)
	if err != nil {
		t.Fatalf("Get after save: %v", err)
	}
	if got != key {
		t.Fatalf("Get mismatch: got %q want %q", got, key)
	}

	// Upsert with a new value.
	const key2 = "sk-xyz-cafebabe-9876543210"
	if err := ks.Save(provider, key2); err != nil {
		t.Fatalf("Save (upsert): %v", err)
	}
	if got, _ := ks.Get(provider); got != key2 {
		t.Fatalf("Get after upsert: got %q want %q", got, key2)
	}

	// Delete.
	if err := ks.Delete(provider); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if has, _ := ks.Has(provider); has {
		t.Fatal("Has after delete should be false")
	}
}
