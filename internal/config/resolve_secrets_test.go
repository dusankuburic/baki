package config

import "testing"

func TestResolveRuntimeSecrets(t *testing.T) {
	t.Run("mints an ephemeral secret when none is configured", func(t *testing.T) {
		cfg := Default()
		cfg.Auth.Secret = ""
		ResolveRuntimeSecrets(cfg)
		if cfg.Auth.Secret == "" || !cfg.Auth.SecretEphemeral {
			t.Fatalf("secret=%q ephemeral=%v, want a minted ephemeral secret", cfg.Auth.Secret, cfg.Auth.SecretEphemeral)
		}
		minted := cfg.Auth.Secret
		ResolveRuntimeSecrets(cfg)
		if cfg.Auth.Secret != minted {
			t.Error("a second call replaced the already resolved secret")
		}
	})
	t.Run("keeps an operator-provided secret", func(t *testing.T) {
		cfg := Default()
		cfg.Auth.Secret = "operator-provided-secret-value-0123456789"
		ResolveRuntimeSecrets(cfg)
		if cfg.Auth.Secret != "operator-provided-secret-value-0123456789" || cfg.Auth.SecretEphemeral {
			t.Fatalf("secret=%q ephemeral=%v, want the provided secret, not ephemeral", cfg.Auth.Secret, cfg.Auth.SecretEphemeral)
		}
	})
}
