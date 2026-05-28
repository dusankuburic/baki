package config

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
}

// ServerConfig holds HTTP server settings
type ServerConfig struct {
	Host           string
	Port           int
	AllowedOrigins []string // CORS / WebSocket origin allowlist (cloud mode)
	TrustedProxies []string // IPs of trusted reverse proxies for rate limiting
	StaticDir      string   // Directory for static frontend assets
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
	}
}
