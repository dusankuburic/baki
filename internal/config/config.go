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
}

// AuthConfig holds authentication settings
type AuthConfig struct {
	// Enabled controls whether auth middleware is enforced
	Enabled bool
	// Secret is the JWT signing key (required when Enabled == true)
	Secret string
}

// Default returns a sensible local-mode configuration
func Default() *Config {
	return &Config{
		Mode: ModeLocal,
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
