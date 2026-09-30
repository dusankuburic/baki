package storage

import (
	"errors"
	"testing"

	"github.com/zalando/go-keyring"
)

// TestKeyringEntry_FamilyNames pins the keychain target names for a key
// family (Part 9): the default Azure resource keeps the historical
// "apikey:azure-foundry" entry, every other resource gets
// "apikey:azure-foundry/<id>", and a "/" passes through the name builder
// unchanged — Windows Credential Manager, the macOS keychain and the
// Secret Service all accept it in a target name.
func TestKeyringEntry_FamilyNames(t *testing.T) {
	for _, tc := range []struct{ scope, provider, want string }{
		{"", "azure-foundry", "apikey:azure-foundry"},
		{"", "azure-foundry/abc0123456", "apikey:azure-foundry/abc0123456"},
		{"user-1", "azure-foundry", "apikey:user-1:azure-foundry"},
		{"user-1", "azure-foundry/abc0123456", "apikey:user-1:azure-foundry/abc0123456"},
	} {
		if got := keyringEntry(tc.scope, tc.provider); got != tc.want {
			t.Errorf("keyringEntry(%q, %q) = %q, want %q", tc.scope, tc.provider, got, tc.want)
		}
	}
	// Distinct keys never share an entry.
	seen := map[string]string{}
	for _, p := range []string{"azure-foundry", "azure-foundry/abc0123456", "azure-foundry/abc0123457", "azure-foundryX"} {
		name := keyringEntry("", p)
		if prev, dup := seen[name]; dup {
			t.Errorf("%q and %q share the entry %q", prev, p, name)
		}
		seen[name] = p
	}
}

// TestKeyringStore_SlashProviderRoundTrips saves, reads, checks and deletes
// a family member through the real keyringStore against go-keyring's
// in-memory mock — no OS keychain is touched — and checks the entry lands
// under exactly the name the builder produced, apart from the default
// resource's entry.
func TestKeyringStore_SlashProviderRoundTrips(t *testing.T) {
	keyring.MockInit()
	s := NewKeyringSecretStore()
	const (
		member = "azure-foundry/abc0123456"
		doc    = `{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key","key":"k"}}`
	)
	if err := s.Save("", member, doc); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if got, err := s.Get("", member); err != nil || got != doc {
		t.Fatalf("Get = (%q, %v), want the saved document", got, err)
	}
	if has, err := s.Has("", member); err != nil || !has {
		t.Errorf("Has = (%v, %v), want true", has, err)
	}
	if raw, err := keyring.Get(keyringService, "apikey:"+member); err != nil || raw != doc {
		t.Errorf("the keychain entry %q = (%q, %v), want the document under that exact name", "apikey:"+member, raw, err)
	}
	if _, err := s.Get("", "azure-foundry"); !errors.Is(err, ErrSecretNotFound) {
		t.Errorf("the default resource's entry = %v, want not found (a member is its own entry)", err)
	}
	if err := s.Delete("", member); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if has, _ := s.Has("", member); has {
		t.Error("the entry survived Delete")
	}
}
