package storage

import (
	"errors"
	"testing"

	"github.com/zalando/go-keyring"
)

// R-provider-2: an unreachable OS keychain must not report a removal that did not happen. Get and Has keep the
// headless behaviour (an absent backend reads as no key), but Delete answers ErrSecretStorageUnavailable, so the
// settings form keeps showing the key and the user is told why.
func TestKeyringStore_UnavailableBackend(t *testing.T) {
	keyring.MockInitWithError(errors.New("org.freedesktop.secrets: no such service"))
	t.Cleanup(keyring.MockInit)
	s := NewKeyringSecretStore()

	if err := s.Delete("", "openai"); !errors.Is(err, ErrSecretStorageUnavailable) {
		t.Errorf("Delete = %v, want ErrSecretStorageUnavailable", err)
	}
	if err := s.Save("", "openai", "sk-1"); !errors.Is(err, ErrSecretStorageUnavailable) {
		t.Errorf("Save = %v, want ErrSecretStorageUnavailable", err)
	}
	if _, err := s.Get("", "openai"); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("Get = %v, want ErrSecretNotFound (the headless behaviour)", err)
	}
	if has, err := s.Has("", "openai"); has || err != nil {
		t.Errorf("Has = (%v, %v), want (false, nil) (the headless behaviour)", has, err)
	}
}

// Deleting a key the keychain does not hold is still a no-op.
func TestKeyringStore_DeleteMissingIsNoOp(t *testing.T) {
	keyring.MockInit()
	if err := NewKeyringSecretStore().Delete("", "never-saved"); err != nil {
		t.Errorf("Delete of a missing key = %v, want nil", err)
	}
}
