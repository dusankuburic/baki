package app

import (
	"errors"
	"strings"
	"testing"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/platform/secret"
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
		{"dedicated encryption key wins", config.AuthConfig{EncryptionKey: secret.New("enc"), Secret: secret.New("jwt")}, "enc"},
		{"operator auth secret is the legacy fallback", config.AuthConfig{Secret: secret.New("jwt")}, "jwt"},
		{"minted per-process secret never qualifies", config.AuthConfig{Secret: secret.New("minted"), SecretEphemeral: true}, ""},
		{"encryption key still wins over a minted secret", config.AuthConfig{EncryptionKey: secret.New("enc"), Secret: secret.New("minted"), SecretEphemeral: true}, "enc"},
		{"nothing configured", config.AuthConfig{}, ""},
	}
	for _, tc := range cases {
		if got := keystoreKey(tc.auth).Reveal(); got != tc.want {
			t.Errorf("%s: keystoreKey = %q, want %q", tc.name, got, tc.want)
		}
	}
}

// noKeyStores is a database whose encrypted keystore cannot be built.
type noKeyStores struct{}

func (noKeyStores) NewEncryptedKeyStore([]byte, ...[]byte) (*storagedb.EncryptedKeyStore, error) {
	return nil, errors.New("no keystore here")
}

// TestProvideKeyStore_DatabaseCapabilitiesNeedTheDatabaseKeystore: the organisation credential store and the key-family
// lister exist only with the encrypted database keystore; with the OS keyring both are nil (constructing the keyring
// store touches no keychain).
func TestProvideKeyStore_DatabaseCapabilitiesNeedTheDatabaseKeystore(t *testing.T) {
	keyringOnly := func(name string, res KeyStoreResult, err error) {
		t.Helper()
		if err != nil || res.Keys == nil || res.OrgKeys != nil || res.KeyFamilies != nil || res.KeyReseal != nil {
			t.Errorf("%s: keys %T, org keys %v, families %v, err %v; want the keyring alone", name, res.Keys, res.OrgKeys, res.KeyFamilies, err)
		}
	}
	cfg := config.Default()
	res, err := provideKeyStore(cfg, nil)
	keyringOnly("no database", res, err)
	res, err = provideKeyStore(cfg, noKeyStores{})
	keyringOnly("a database without an encryption key", res, err)
	cfg.Auth.EncryptionKey = secret.New("enc")
	if _, err := provideKeyStore(cfg, noKeyStores{}); err == nil {
		t.Error("a keystore that cannot be built must fail the boot")
	}
}

// TestKeystorePreviousKeys: the keys PAD_ENCRYPTION_KEY replaced open what they sealed, in the configured order, and
// only beside a dedicated key (config validation refuses them otherwise; the auth-secret fallback never gets them).
func TestKeystorePreviousKeys(t *testing.T) {
	got := keystorePreviousKeys(config.AuthConfig{EncryptionKey: secret.New("enc"), EncryptionKeyPrevious: []secret.Value{secret.New("newer"), secret.New("older")}})
	if len(got) != 2 || string(got[0]) != "newer" || string(got[1]) != "older" {
		t.Errorf("keystorePreviousKeys = %q, want newer then older", got)
	}
	if got := keystorePreviousKeys(config.AuthConfig{Secret: secret.New("jwt"), EncryptionKeyPrevious: []secret.Value{secret.New("old")}}); got != nil {
		t.Errorf("previous keys beside the auth-secret fallback = %q, want none", got)
	}
}

// builtKeyStores is a database whose encrypted keystore builds (a zero store: nothing here reads it).
type builtKeyStores struct{ ks *storagedb.EncryptedKeyStore }

func (b builtKeyStores) NewEncryptedKeyStore([]byte, ...[]byte) (*storagedb.EncryptedKeyStore, error) {
	return b.ks, nil
}

// TestProvideKeyStore_RequestScopedReads (R-provider-19): the keystore's request-scoped reads — a scope's keys in
// one read, a key under the request's context — are fx values of their own, so no service type-asserts the store
// for them. The database keystore is both its own and the organisation's; the OS keyring reads personal keys only.
func TestProvideKeyStore_RequestScopedReads(t *testing.T) {
	cfg := config.Default()
	res, err := provideKeyStore(cfg, nil)
	if err != nil || res.KeyReads == nil || res.OrgKeyReads != nil {
		t.Errorf("keyring: key reads %v, org key reads %v, err %v; want the keyring's reads and no org reads", res.KeyReads, res.OrgKeyReads, err)
	}

	ks := new(storagedb.EncryptedKeyStore)
	cfg.Auth.EncryptionKey = secret.New("enc")
	res, err = provideKeyStore(cfg, builtKeyStores{ks: ks})
	if err != nil {
		t.Fatal(err)
	}
	if any(res.KeyReads) != any(ks) {
		t.Errorf("database: key reads %T, want the database keystore", res.KeyReads)
	}
	if any(res.OrgKeyReads) != any(ks) {
		t.Errorf("database: org key reads %T, want the database keystore", res.OrgKeyReads)
	}
}

// recordingKeyStores records the keys the keystore is built with.
type recordingKeyStores struct {
	current  string
	previous []string
}

func (r *recordingKeyStores) NewEncryptedKeyStore(secret []byte, previous ...[]byte) (*storagedb.EncryptedKeyStore, error) {
	r.current = string(secret)
	for _, p := range previous {
		r.previous = append(r.previous, string(p))
	}
	return nil, errors.New("recorded")
}

// TestProvideKeyStore_OpensWithThePreviousKeys: the database keystore seals with PAD_ENCRYPTION_KEY and also opens
// with every key in PAD_ENCRYPTION_KEY_PREVIOUS, so a rotation does not make stored provider keys unreadable.
func TestProvideKeyStore_OpensWithThePreviousKeys(t *testing.T) {
	cfg := config.Default()
	cfg.Auth.EncryptionKey = secret.New("enc")
	cfg.Auth.EncryptionKeyPrevious = []secret.Value{secret.New("old-1"), secret.New("old-2")}
	rec := &recordingKeyStores{}
	_, _ = provideKeyStore(cfg, rec)
	if rec.current != "enc" || len(rec.previous) != 2 || rec.previous[0] != "old-1" || rec.previous[1] != "old-2" {
		t.Errorf("keystore built with current %q previous %q, want enc and old-1, old-2", rec.current, rec.previous)
	}
}

// TestProvideKeyStore_CloudFallbackLogsTheDatedDeprecation (R-security-7): a cloud boot whose keystore falls back to
// PAD_AUTH_SECRET logs the dated deprecation with what to set, never the secret; one with PAD_ENCRYPTION_KEY does not.
func TestProvideKeyStore_CloudFallbackLogsTheDatedDeprecation(t *testing.T) {
	logs := captureLogs(t)
	cfg := config.Default()
	cfg.Mode = config.ModeCloud
	cfg.Auth.Enabled = true
	cfg.Auth.Secret = secret.New("the-operators-jwt-secret-0123456789abcdef")
	_, _ = provideKeyStore(cfg, &recordingKeyStores{})
	out := logText(logs)
	if !strings.Contains(out, "DEPRECATED: PAD_ENCRYPTION_KEY is unset") || !strings.Contains(out, config.EncryptionKeyFallbackRemoval) ||
		!strings.Contains(out, "PAD_ENCRYPTION_KEY_PREVIOUS=") {
		t.Errorf("the cloud fallback boot logged no dated deprecation with what to set: %s", out)
	}
	if strings.Contains(out, cfg.Auth.Secret.Reveal()) {
		t.Error("the boot log carries the auth secret")
	}

	logs = captureLogs(t)
	cfg.Auth.EncryptionKey = secret.New("a-dedicated-encryption-key-0123456789abcdef")
	_, _ = provideKeyStore(cfg, &recordingKeyStores{})
	if out := logText(logs); strings.Contains(out, "DEPRECATED") {
		t.Errorf("a boot with PAD_ENCRYPTION_KEY logged the deprecation: %s", out)
	}
}

// logText is everything the capture holds.
func logText(l *logCapture) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
