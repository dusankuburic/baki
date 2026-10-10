package config

import (
	"testing"

	"pad-analyzer/internal/platform/secret"
)

func TestResolveRuntimeSecrets(t *testing.T) {
	t.Run("mints an ephemeral secret when none is configured", func(t *testing.T) {
		cfg := Default()
		cfg.Auth.Secret = secret.Value{}
		ResolveRuntimeSecrets(cfg)
		if cfg.Auth.Secret.Reveal() == "" || !cfg.Auth.SecretEphemeral {
			t.Fatalf("secret=%q ephemeral=%v, want a minted ephemeral secret", cfg.Auth.Secret, cfg.Auth.SecretEphemeral)
		}
		minted := cfg.Auth.Secret.Reveal()
		ResolveRuntimeSecrets(cfg)
		if cfg.Auth.Secret.Reveal() != minted {
			t.Error("a second call replaced the already resolved secret")
		}
	})
	t.Run("keeps an operator-provided secret", func(t *testing.T) {
		cfg := Default()
		cfg.Auth.Secret = secret.New("operator-provided-secret-value-0123456789")
		ResolveRuntimeSecrets(cfg)
		if cfg.Auth.Secret.Reveal() != "operator-provided-secret-value-0123456789" || cfg.Auth.SecretEphemeral {
			t.Fatalf("secret=%q ephemeral=%v, want the provided secret, not ephemeral", cfg.Auth.Secret, cfg.Auth.SecretEphemeral)
		}
	})
}
