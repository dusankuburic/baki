package main

import (
	"testing"

	"pad-analyzer/internal/config"
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
