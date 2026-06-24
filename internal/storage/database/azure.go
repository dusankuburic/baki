package database

import (
	"context"
	"database/sql/driver"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/Azure/azure-sdk-for-go/sdk/azcore"
	"github.com/Azure/azure-sdk-for-go/sdk/azcore/policy"
	"github.com/Azure/azure-sdk-for-go/sdk/azidentity"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/stdlib"
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

// GetAccessToken returns the full access token (including expiry) for the
// PostgreSQL scope.
func (p *azureTokenProvider) GetAccessToken(ctx context.Context) (azcore.AccessToken, error) {
	slog.Debug("azure: fetching Entra ID token for PostgreSQL")
	token, err := p.cred.GetToken(ctx, policy.TokenRequestOptions{
		Scopes: []string{azurePostgresScope},
	})
	if err != nil {
		return azcore.AccessToken{}, fmt.Errorf("azure: failed to get token: %w", err)
	}
	return token, nil
}

func (p *azureTokenProvider) GetToken(ctx context.Context) (string, error) {
	at, err := p.GetAccessToken(ctx)
	if err != nil {
		return "", err
	}
	return at.Token, nil
}

// azureMIConnector is a database/sql driver.Connector that injects a freshly
// fetched (and cached) Entra ID token as the connection password on every new
// pooled connection. This replaces mutating a shared pgx.ConnConfig.Password
// field from a background goroutine, which raced with the pgx driver reading
// that field when opening connections. The token is cached and refreshed under
// mu, so the read happens on the connecting goroutine's own synchronized path.
type azureMIConnector struct {
	provider *azureTokenProvider
	template *pgx.ConnConfig // Password is set per-connect from the cached token

	mu      sync.Mutex
	token   string
	expires time.Time
}

func newAzureMIConnector(provider *azureTokenProvider, template *pgx.ConnConfig) *azureMIConnector {
	return &azureMIConnector{provider: provider, template: template}
}

// currentToken returns a valid token, refreshing when within 5 minutes of
// expiry (Entra tokens last ~24h).
func (c *azureMIConnector) currentToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == "" || time.Until(c.expires) < 5*time.Minute {
		at, err := c.provider.GetAccessToken(ctx)
		if err != nil {
			return "", err
		}
		c.token = at.Token
		c.expires = at.ExpiresOn
		slog.Info("azure: refreshed managed-identity token for postgres", "expires", at.ExpiresOn)
	}
	return c.token, nil
}

func (c *azureMIConnector) Connect(ctx context.Context) (driver.Conn, error) {
	tok, err := c.currentToken(ctx)
	if err != nil {
		return nil, err
	}
	cfg := c.template.Copy()
	cfg.Password = tok
	return stdlib.GetConnector(*cfg).Connect(ctx)
}

func (c *azureMIConnector) Driver() driver.Driver { return stdlib.GetDefaultDriver() }

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
