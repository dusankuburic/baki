package padcloud

import (
	"strings"
	"testing"
)

// TestNewDBTokenStore_EmptyKeyFailsClosed verifies the fix for silent plaintext
// fallback: an empty encryption key must return an error (not a warn-and-continue
// plaintext store), because a Power Platform refresh token grants tenant-wide
// access and must never be persisted unencrypted.
func TestNewDBTokenStore_EmptyKeyFailsClosed(t *testing.T) {
	_, err := NewDBTokenStore(nil, nil)
	if err == nil {
		t.Fatal("expected an error for an empty encryption key, got nil (plaintext fallback)")
	}
	if !strings.Contains(err.Error(), "empty") {
		t.Errorf("expected the error to mention the empty key, got %q", err.Error())
	}
	_, err = NewDBTokenStore(nil, []byte{})
	if err == nil {
		t.Fatal("expected an error for a nil-byte encryption key, got nil")
	}
}

// TestNewDBTokenStore_ValidKeySucceeds confirms a valid key constructs a store
// (the AEAD is derived without touching the DB, so a nil DB is fine here).
func TestNewDBTokenStore_ValidKeySucceeds(t *testing.T) {
	s, err := NewDBTokenStore(nil, []byte("a-valid-deployment-encryption-key"))
	if err != nil {
		t.Fatalf("expected no error for a valid key, got %v", err)
	}
	if s == nil {
		t.Fatal("expected a non-nil store for a valid key")
	}
}
