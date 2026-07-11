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
	"golang.org/x/sync/singleflight"
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
	// sf collapses concurrent refreshes into one token fetch. Without it, the
	// mutex was held across the (potentially slow) Entra/IMDS call, so every
	// new pooled connection stalled behind a single refresh — precisely when
	// the pool is churning at a token boundary or right after invalidation.
	sf singleflight.Group
}

func newAzureMIConnector(provider *azureTokenProvider, template *pgx.ConnConfig) *azureMIConnector {
	return &azureMIConnector{provider: provider, template: template}
}

// currentToken returns a valid token, refreshing when within 5 minutes of
// expiry (Entra tokens last ~24h). The cache read and write are guarded by mu,
// but the network fetch happens outside the lock (via singleflight), so a
// refresh doesn't serialize every other connecting goroutine.
func (c *azureMIConnector) currentToken(ctx context.Context) (string, error) {
	c.mu.Lock()
	tok, exp := c.token, c.expires
	c.mu.Unlock()
	if tok != "" && time.Until(exp) >= 5*time.Minute {
		return tok, nil
	}

	v, err, _ := c.sf.Do("token", func() (any, error) {
		at, err := c.provider.GetAccessToken(ctx)
		if err != nil {
			return "", err
		}
		c.mu.Lock()
		c.token = at.Token
		c.expires = at.ExpiresOn
		c.mu.Unlock()
		slog.Info("azure: refreshed managed-identity token for postgres", "expires", at.ExpiresOn)
		return at.Token, nil
	})
	if err != nil {
		return "", err
	}
	return v.(string), nil
}

// invalidate drops the cached token if it is still the one that just failed,
// so the next currentToken call fetches a fresh one. The comparison guards
// against clearing a newer token another goroutine already refreshed.
func (c *azureMIConnector) invalidate(failed string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.token == failed {
		c.token = ""
		c.expires = time.Time{}
	}
}

// isPgAuthError reports whether err is a PostgreSQL authentication failure
// (SQLSTATE class 28): the server rejected the credential itself, as opposed
// to a network or protocol error.
func isPgAuthError(err error) bool {
	return isPgErrCode(err, "28000") || isPgErrCode(err, "28P01")
}

func (c *azureMIConnector) Connect(ctx context.Context) (driver.Conn, error) {
	tok, err := c.currentToken(ctx)
	if err != nil {
		return nil, err
	}
	conn, err := c.connectWith(ctx, tok)
	if err == nil || !isPgAuthError(err) {
		return conn, err
	}

	// The server rejected the token even though it hasn't expired — it was
	// invalidated out-of-band (identity reassigned, RBAC change, credential
	// rotation). Without this, every new connection keeps replaying the dead
	// cached token until its natural expiry (~24h). Drop it and retry once
	// with a freshly fetched token; if the upstream credential cache hands
	// back the same token, surface the original auth error rather than loop.
	c.invalidate(tok)
	fresh, ferr := c.currentToken(ctx)
	if ferr != nil || fresh == tok {
		return nil, err
	}
	slog.Warn("azure: postgres rejected cached managed-identity token; retrying with a fresh token", "error", err)
	return c.connectWith(ctx, fresh)
}

func (c *azureMIConnector) connectWith(ctx context.Context, token string) (driver.Conn, error) {
	cfg := c.template.Copy()
	cfg.Password = token
	return stdlib.GetConnector(*cfg).Connect(ctx)
}

func (c *azureMIConnector) Driver() driver.Driver { return stdlib.GetDefaultDriver() }
