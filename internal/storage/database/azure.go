package database

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/jackc/pgx/v5"
)

const azurePostgresScope = "https://ossrdbms-aad.database.windows.net/.default"

// azureTokenProvider fetches a fresh Entra ID token for Azure Database for PostgreSQL.
type azureTokenProvider struct {
	cred azcore.TokenCredential
}

func newAzureTokenProvider() (*azureTokenProvider, error) {
	cred, err := azidentity.NewDefaultAzureCredential(nil)
	if err != nil {
		return nil, fmt.Errorf("azure: failed to obtain credential: %w", err)
	}
	return &azureTokenProvider{cred: cred}, nil
}

func (p *azureTokenProvider) GetToken(ctx context.Context) (string, error) {
	slog.Debug("azure: fetching Entra ID token for PostgreSQL")
	token, err := p.cred.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{azurePostgresScope},
	})
	if err != nil {
		return "", fmt.Errorf("azure: failed to get token: %w", err)
	}
	return token.Token, nil
}

// AzureConfigHook returns a function that can be used with stdlib.RegisterConnConfig
// to inject Entra ID tokens as passwords for Managed Identity connections.
func AzureConfigHook(ctx context.Context, cfg *pgx.ConnConfig) error {
	if cfg.Password != "managed-identity" {
		return nil
	}

	provider, err := newAzureTokenProvider()
	if err != nil {
		return err
	}

	slog.Info("azure: enabling Managed Identity for PostgreSQL connection", "host", cfg.Host, "user", cfg.User)

	// Since tokens expire, we fetch a new one for this connection.
	token, err := provider.GetToken(ctx)
	if err != nil {
		return err
	}
	cfg.Password = token
	return nil
}
