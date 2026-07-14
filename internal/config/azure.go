package config

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/Azure/azure-sdk-for-go/sdk/security/keyvault/azsecrets"
)

// ResolveAzureSecrets fetches sensitive configuration from Azure Key Vault if KeyVaultURL is set.
// It uses the DefaultAzureCredential, which supports Managed Identity in Azure and
// local developer credentials (Azure CLI, etc.).
//
// Secrets are mapped from their Key Vault names to Config fields:
//   - "pad-auth-secret"          -> Auth.Secret
//   - "pad-database-url"         -> Storage.DatabaseURL
//   - "pad-encryption-key"       -> Auth.EncryptionKey
//   - "pad-sso-client-secret"    -> Auth.SSO.ClientSecret
//   - "pad-smtp-password"        -> Email.Password
//   - "pad-metrics-token"        -> Server.MetricsToken
//
// If a secret is already set in the Config (from ENV or JSON), the Key Vault value
// is ignored to allow for local overrides.
func ResolveAzureSecrets(ctx context.Context, cfg *Config) error {
	if cfg.Server.KeyVaultURL == "" {
		return nil
	}

	slog.Info("resolving secrets from Azure Key Vault", "url", cfg.Server.KeyVaultURL)

	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return fmt.Errorf("azure: failed to obtain credential: %w", err)
	}

	client, err := azsecrets.NewClient(cfg.Server.KeyVaultURL, cred, nil)
	if err != nil {
		return fmt.Errorf("azure: failed to create keyvault client: %w", err)
	}

	// List of secrets to fetch and their target fields
	// format: {kvName, targetPtr}
	mappings := []struct {
		kvName string
		target *string
	}{
		{"pad-auth-secret", &cfg.Auth.Secret},
		{"pad-database-url", &cfg.Storage.DatabaseURL},
		{"pad-encryption-key", &cfg.Auth.EncryptionKey},
		{"pad-sso-client-secret", &cfg.Auth.SSO.ClientSecret},
		{"pad-smtp-password", &cfg.Email.Password},
		{"pad-metrics-token", &cfg.Server.MetricsToken},
	}

	for _, m := range mappings {
		// Skip if already set
		if *m.target != "" {
			continue
		}

		resp, err := client.GetSecret(ctx, m.kvName, "", nil)
		if err != nil {
			// A missing secret (404) is logged and skipped: not every mapping
			// is required for every deployment. Use the SDK's typed
			// ResponseError rather than string-matching the message, so this
			// survives SDK version bumps and localization.
			var respErr *azcore.ResponseError
			if errors.As(err, &respErr) && respErr.StatusCode == http.StatusNotFound {
				slog.Warn("azure: secret not found in Key Vault", "name", m.kvName)
				continue
			}
			return fmt.Errorf("azure: failed to get secret %q: %w", m.kvName, err)
		}

		if resp.Value != nil {
			*m.target = *resp.Value
			slog.Info("azure: resolved secret from Key Vault", "name", m.kvName)
		}
	}

	return nil
}
