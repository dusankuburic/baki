package config

import "pad-analyzer/internal/models"

// DeploymentMode indicates how the application is deployed
type DeploymentMode string

const (
	ModeLocal DeploymentMode = "local"
	ModeCloud DeploymentMode = "cloud"
)

// StorageBackend indicates which storage implementation to use
type StorageBackend string

const (
	StorageLocal    StorageBackend = "local"
	StorageDatabase StorageBackend = "database"
)

// Config holds the complete application configuration
type Config struct {
	Mode    DeploymentMode
	Server  ServerConfig
	Storage StorageConfig
	Auth    AuthConfig
	Runtime RuntimeConfig
	Log     models.LogConfig
}

// ServerConfig holds HTTP server settings
type ServerConfig struct {
	Host           string
	Port           int
	AllowedOrigins []string // CORS / WebSocket origin allowlist (cloud mode)
	TrustedProxies []string // IPs of trusted reverse proxies for rate limiting
	StaticDir      string   // Directory for static frontend assets
	// KeyVaultURL is the URL of the Azure Key Vault to fetch secrets from.
	// When set, secrets like PAD_AUTH_SECRET and PAD_DATABASE_URL will be
	// retrieved from Key Vault if not already provided via ENV.
	KeyVaultURL string
	// TLSCert and TLSKey are paths to PEM-encoded cert/key files. When both
	// are set the server uses ListenAndServeTLS directly. Leave empty when
	// terminating TLS at a reverse proxy and set BehindProxy=true instead.
	TLSCert string
	TLSKey  string
	// BehindProxy declares that a trusted TLS-terminating reverse proxy is
	// in front. It is the operator's "I know what I'm doing" flag — without
	// it (and without TLSCert/TLSKey), a cloud-mode deployment with auth
	// enabled refuses to start, to prevent accidental plaintext credentials.
	BehindProxy bool
}

// StorageConfig holds storage backend settings
type StorageConfig struct {
	// DataDir is the root directory for local file storage
	DataDir string
	Backend StorageBackend
	// DatabaseURL is used when Backend == StorageDatabase
	DatabaseURL string

	// Connection pool settings for the PostgreSQL backend.
	// Zero values mean "use driver defaults" (see database.DefaultConfig).
	// Configure via PAD_DB_MAX_OPEN_CONNS, PAD_DB_MAX_IDLE_CONNS,
	// and PAD_DB_CONN_MAX_LIFETIME (e.g. "1h", "30m").
	DBMaxOpenConns    int
	DBMaxIdleConns    int
	DBConnMaxLifetime string // duration string, e.g. "1h"

	// Azure Blob Storage settings (optional). Used when Backend == StorageDatabase.
	AzureStorageAccount   string
	AzureStorageContainer string
}

type RuntimeConfig struct {
	RateLimitGeneralRPS    float64
	RateLimitGeneralBurst  float64
	RateLimitAuthRPS       float64
	RateLimitAuthBurst     float64
	RateLimitExpensiveRPS  float64
	RateLimitExpensiveBurst float64
	RateLimitChatRPS       float64
	RateLimitChatBurst     float64
	RateLimitUploadRPS     float64
	RateLimitUploadBurst   float64
	CircuitBreakerFailures int
	CircuitBreakerOpenDur  string
	RetryMaxAttempts       int
	RetryBaseDelay         string
	JWTAccessTTL           string
	JWTRefreshTTL          string
	OTLPEndpoint           string
	RequestTimeout         string
}

func DefaultRuntimeConfig() RuntimeConfig {
	return RuntimeConfig{
		RateLimitGeneralRPS:    1.0,
		RateLimitGeneralBurst:  20,
		RateLimitAuthRPS:       5.0 / 60.0,
		RateLimitAuthBurst:     5,
		RateLimitExpensiveRPS:  2,
		RateLimitExpensiveBurst: 5,
		RateLimitChatRPS:       3,
		RateLimitChatBurst:     10,
		RateLimitUploadRPS:     1,
		RateLimitUploadBurst:   3,
		CircuitBreakerFailures: 5,
		CircuitBreakerOpenDur:  "30s",
		RetryMaxAttempts:       3,
		RetryBaseDelay:         "500ms",
		JWTAccessTTL:           "15m",
		JWTRefreshTTL:          "24h",
		OTLPEndpoint:           "",
		RequestTimeout:         "30s",
	}
}

type AuthConfig struct {
	// Enabled controls whether auth middleware is enforced
	Enabled bool
	// Secret is the JWT signing key (required when Enabled == true)
	Secret string
	// SSO configures account-level OIDC login (cloud mode only)
	SSO SSOConfig
}

// SSOConfig holds OIDC single-sign-on settings. SSO is enabled when
// IssuerURL, ClientID, and RedirectURL are all set (and auth is enabled).
// Works with any OIDC-compliant IdP via discovery: Microsoft Entra ID,
// Google, Okta, Keycloak, etc.
type SSOConfig struct {
	// IssuerURL is the OIDC issuer, e.g.
	// https://login.microsoftonline.com/{tenant}/v2.0 or https://accounts.google.com.
	// The discovery document is fetched from {IssuerURL}/.well-known/openid-configuration.
	IssuerURL string
	// ClientID is the OAuth2 client ID from the IdP app registration.
	ClientID string
	// ClientSecret is optional — leave empty for public clients using PKCE only.
	ClientSecret string
	// RedirectURL is the absolute callback URL registered with the IdP,
	// e.g. https://app.example.com/api/auth/sso/callback.
	RedirectURL string
	// ProviderName is the display label and the identity_links.provider key.
	// Defaults to "sso". Changing it after users have linked breaks their links.
	ProviderName string
}

// Enabled reports whether SSO is fully configured.
func (s SSOConfig) Enabled() bool {
	return s.IssuerURL != "" && s.ClientID != "" && s.RedirectURL != ""
}

// Default returns a sensible local-mode configuration
func Default() *Config {
	return &Config{
		Mode:    ModeLocal,
		Runtime: DefaultRuntimeConfig(),
		Server: ServerConfig{
			Host: "localhost",
			Port: 0, // 0 = OS-assigned ephemeral port (current behaviour)
		},
		Storage: StorageConfig{
			Backend: StorageLocal,
		},
		Auth: AuthConfig{
			Enabled: false,
		},
		Log: models.LogConfig{
			Level: "debug",
		},
	}
}
