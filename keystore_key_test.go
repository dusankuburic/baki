package main

import (
	"errors"
	"testing"

	"pad-analyzer/internal/config"
	storagedb "pad-analyzer/internal/storage/database"
)

// TestKeystoreKey: the database keystore never keys off a secret minted for this process (stored provider keys
// would be unreadable after the next restart); an operator-provided key or secret still works.
func TestKeystoreKey(t *testing.T) {
	cases := []struct {
		name string
		auth config.AuthConfig
		want string
	}{
		{"dedicated encryption key wins", config.AuthConfig{EncryptionKey: "enc", Secret: "jwt"}, "enc"},
		{"operator auth secret is the legacy fallback", config.AuthConfig{Secret: "jwt"}, "jwt"},
		{"minted per-process secret never qualifies", config.AuthConfig{Secret: "minted", SecretEphemeral: true}, ""},
		{"encryption key still wins over a minted secret", config.AuthConfig{EncryptionKey: "enc", Secret: "minted", SecretEphemeral: true}, "enc"},
		{"nothing configured", config.AuthConfig{}, ""},
	}
	for _, tc := range cases {
		if got := keystoreKey(tc.auth); got != tc.want {
			t.Errorf("%s: keystoreKey = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// noKeyStores is a database whose encrypted keystore cannot be built.
type noKeyStores struct{}

func (noKeyStores) NewEncryptedKeyStore([]byte) (*storagedb.EncryptedKeyStore, error) {
	return nil, errors.New("no keystore here")
}

// TestProvideKeyStore_DatabaseCapabilitiesNeedTheDatabaseKeystore: the organisation credential store and the key-family
// lister exist only with the encrypted database keystore; with the OS keyring both are nil (constructing the keyring
// store touches no keychain).
func TestProvideKeyStore_DatabaseCapabilitiesNeedTheDatabaseKeystore(t *testing.T) {
	keyringOnly := func(name string, res KeyStoreResult, err error) {
		t.Helper()
		if err != nil || res.Keys == nil || res.OrgKeys != nil || res.KeyFamilies != nil {
			t.Errorf("%s: keys %T, org keys %v, families %v, err %v; want the keyring alone", name, res.Keys, res.OrgKeys, res.KeyFamilies, err)
		}
	}
	cfg := config.Default()
	res, err := provideKeyStore(cfg, nil)
	keyringOnly("no database", res, err)
	res, err = provideKeyStore(cfg, noKeyStores{})
	keyringOnly("a database without an encryption key", res, err)
	cfg.Auth.EncryptionKey = "enc"
	if _, err := provideKeyStore(cfg, noKeyStores{}); err == nil {
		t.Error("a keystore that cannot be built must fail the boot")
	}
}
