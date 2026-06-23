package config

import (
	"context"
	"testing"
	"time"
)

func TestResolveAzureSecrets_EmptyKeyVaultURL(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Server.KeyVaultURL = ""

	orig := cfg.Auth.Secret
	err := ResolveAzureSecrets(context.Background(), cfg)
	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if cfg.Auth.Secret != orig {
		t.Error("Auth.Secret should not change when KeyVaultURL is empty")
	}
}

func TestResolveAzureSecrets_CancelledContext(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Server.KeyVaultURL = "https://fake-kv.vault.azure.net/"
	cfg.Auth.Secret = ""
	cfg.Storage.DatabaseURL = ""

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := ResolveAzureSecrets(ctx, cfg)
	if err == nil {
		t.Fatal("expected error with cancelled context, got nil")
	}
}

func TestResolveAzureSecrets_SkipAlreadySet(t *testing.T) {
	t.Parallel()

	cfg := Default()
	cfg.Server.KeyVaultURL = "https://fake-kv.vault.azure.net/"
	cfg.Auth.Secret = "pre-existing-secret"
	cfg.Storage.DatabaseURL = "pre-existing-db-url"

	err := ResolveAzureSecrets(context.Background(), cfg)
	if err != nil {
		t.Fatalf("expected no error when all values already set (KV calls skipped), got %v", err)
	}

	if cfg.Auth.Secret != "pre-existing-secret" {
		t.Errorf("Auth.Secret should remain 'pre-existing-secret', got %q", cfg.Auth.Secret)
	}
	if cfg.Storage.DatabaseURL != "pre-existing-db-url" {
		t.Errorf("Storage.DatabaseURL should remain 'pre-existing-db-url', got %q", cfg.Storage.DatabaseURL)
	}
}

func TestResolveAzureSecrets_PartialValues(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name                   string
		authSecret             string
		databaseURL            string
		expectAuthUntouched    bool
		expectDBURLOverwritten bool
	}{
		{
			name:                "auth set, db empty",
			authSecret:          "already-set",
			databaseURL:         "",
			expectAuthUntouched: true,
		},
		{
			name:                "auth empty, db set",
			authSecret:          "",
			databaseURL:         "already-set",
			expectAuthUntouched: false,
		},
		{
			name:                "both set",
			authSecret:          "already-set",
			databaseURL:         "already-set",
			expectAuthUntouched: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			cfg := Default()
			cfg.Server.KeyVaultURL = "https://fake-kv.vault.azure.net/"
			cfg.Auth.Secret = tt.authSecret
			cfg.Storage.DatabaseURL = tt.databaseURL

			ctx, cancel := context.WithCancel(context.Background())
			cancel()

			_ = ResolveAzureSecrets(ctx, cfg)

			if tt.expectAuthUntouched && cfg.Auth.Secret != tt.authSecret {
				t.Errorf("Auth.Secret should remain %q, got %q", tt.authSecret, cfg.Auth.Secret)
			}
			if tt.databaseURL != "" && cfg.Storage.DatabaseURL != tt.databaseURL {
				t.Errorf("Storage.DatabaseURL should remain %q, got %q", tt.databaseURL, cfg.Storage.DatabaseURL)
			}
		})
	}
}

func TestResolveAzureSecrets_DefaultConfigUnchanged(t *testing.T) {
	t.Parallel()

	cfg := Default()
	before := cfg.Auth.Secret

	start := time.Now()
	err := ResolveAzureSecrets(context.Background(), cfg)
	elapsed := time.Since(start)

	if err != nil {
		t.Fatalf("expected no error, got %v", err)
	}
	if cfg.Auth.Secret != before {
		t.Error("default config should remain unchanged with empty KeyVaultURL")
	}
	if elapsed > 100*time.Millisecond {
		t.Errorf("empty KeyVaultURL path took too long: %v", elapsed)
	}
}
