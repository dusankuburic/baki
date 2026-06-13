package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
)

// LoadRaw reads configuration from the given JSON file without validating.
// The caller is responsible for calling Validate on the returned Config.
func LoadRaw(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Default(), nil
		}
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}
	cfg := Default()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}
	return cfg, nil
}

// LoadFromEnvRaw reads PAD_* environment variables into a Config without
// validating. The caller is responsible for calling Validate on the result.
// Use this when you need to resolve additional secrets (e.g. Azure Key Vault)
// before the config is validated.
func LoadFromEnvRaw() *Config {
	cfg := Default()
	applyEnvVars(cfg)
	return cfg
}

// Load reads configuration from the given JSON file.
// If the file does not exist it returns Default() without an error.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return Default(), nil
		}
		return nil, fmt.Errorf("config: read %s: %w", path, err)
	}

	cfg := Default()
	if err := json.Unmarshal(data, cfg); err != nil {
		return nil, fmt.Errorf("config: parse %s: %w", path, err)
	}

	if err := Validate(cfg); err != nil {
		return nil, err
	}

	return cfg, nil
}

// Save writes the configuration to the given JSON file.
func Save(cfg *Config, path string) error {
	data, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return fmt.Errorf("config: marshal: %w", err)
	}
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	return nil
}

// LoadFromEnv builds a Config from PAD_* environment variables.
// Recognised variables:
//
//	PAD_MODE                   "local" or "cloud"      (default: "local")
//	PAD_HOST                   bind host               (default: "localhost")
//	PAD_PORT                   bind port               (default: 0 = ephemeral)
//	PAD_STATIC_DIR             static assets directory (default: "")
//	PAD_ALLOWED_ORIGINS        comma-separated origins for CORS/WebSocket
//	PAD_TRUSTED_PROXIES        comma-separated IPs of trusted reverse proxies
//	PAD_DATA_DIR               local storage root
//	PAD_STORAGE                "local" or "database"
//	PAD_DATABASE_URL           postgres DSN            (required when PAD_STORAGE=database)
//	PAD_AUTH_ENABLED           "true" / "false"        (default: "false")
//	PAD_AUTH_SECRET            JWT signing key
//	PAD_DB_MAX_OPEN_CONNS      max open PostgreSQL connections (default: 25)
//	PAD_DB_MAX_IDLE_CONNS      max idle PostgreSQL connections (default: 5)
//	PAD_DB_CONN_MAX_LIFETIME   max connection lifetime (e.g. "1h", "30m"; default: "1h")
func LoadFromEnv() (*Config, error) {
	cfg := Default()
	if err := applyEnvVars(cfg); err != nil {
		return nil, err
	}
	if err := Validate(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// applyEnvVars reads PAD_* environment variables into cfg.
// It returns an error only for syntactically invalid numeric values; all other
// validation is deferred to Validate().
func applyEnvVars(cfg *Config) error {
	if v := os.Getenv("PAD_MODE"); v != "" {
		cfg.Mode = DeploymentMode(v)
	}
	if v := os.Getenv("PAD_HOST"); v != "" {
		cfg.Server.Host = v
	}
	if v := os.Getenv("PAD_STATIC_DIR"); v != "" {
		cfg.Server.StaticDir = v
	}
	if v := os.Getenv("PAD_KEYVAULT_URL"); v != "" {
		cfg.Server.KeyVaultURL = v
	}
	if v := os.Getenv("PAD_ALLOWED_ORIGINS"); v != "" {
		for _, o := range strings.Split(v, ",") {
			if trimmed := strings.TrimSpace(o); trimmed != "" {
				cfg.Server.AllowedOrigins = append(cfg.Server.AllowedOrigins, trimmed)
			}
		}
	}
	if v := os.Getenv("PAD_TRUSTED_PROXIES"); v != "" {
		for _, p := range strings.Split(v, ",") {
			if trimmed := strings.TrimSpace(p); trimmed != "" {
				cfg.Server.TrustedProxies = append(cfg.Server.TrustedProxies, trimmed)
			}
		}
	}
	if v := os.Getenv("PAD_PORT"); v != "" {
		p, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("config: PAD_PORT is not a valid integer: %w", err)
		}
		cfg.Server.Port = p
	}
	if v := os.Getenv("PAD_DATA_DIR"); v != "" {
		cfg.Storage.DataDir = v
	}
	if v := os.Getenv("PAD_STORAGE"); v != "" {
		cfg.Storage.Backend = StorageBackend(v)
	}
	if v := os.Getenv("PAD_DATABASE_URL"); v != "" {
		cfg.Storage.DatabaseURL = v
	}
	if v := os.Getenv("PAD_AUTH_ENABLED"); v != "" {
		cfg.Auth.Enabled = v == "true" || v == "1"
	}
	if v := os.Getenv("PAD_AUTH_SECRET"); v != "" {
		cfg.Auth.Secret = v
	}
	if v := os.Getenv("PAD_SSO_ISSUER"); v != "" {
		cfg.Auth.SSO.IssuerURL = v
	}
	if v := os.Getenv("PAD_SSO_CLIENT_ID"); v != "" {
		cfg.Auth.SSO.ClientID = v
	}
	if v := os.Getenv("PAD_SSO_CLIENT_SECRET"); v != "" {
		cfg.Auth.SSO.ClientSecret = v
	}
	if v := os.Getenv("PAD_SSO_REDIRECT_URL"); v != "" {
		cfg.Auth.SSO.RedirectURL = v
	}
	if v := os.Getenv("PAD_SSO_PROVIDER_NAME"); v != "" {
		cfg.Auth.SSO.ProviderName = v
	}
	if v := os.Getenv("PAD_TLS_CERT"); v != "" {
		cfg.Server.TLSCert = v
	}
	if v := os.Getenv("PAD_TLS_KEY"); v != "" {
		cfg.Server.TLSKey = v
	}
	if v := os.Getenv("PAD_BEHIND_PROXY"); v != "" {
		cfg.Server.BehindProxy = v == "true" || v == "1"
	}
	if v := os.Getenv("PAD_LOG_LEVEL"); v != "" {
		cfg.Log.Level = v
	}
	// PostgreSQL connection pool tuning — allows operators to right-size the pool
	// for the Azure Database for PostgreSQL SKU without a code change.
	if v := os.Getenv("PAD_DB_MAX_OPEN_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("config: PAD_DB_MAX_OPEN_CONNS is not a valid integer: %w", err)
		}
		cfg.Storage.DBMaxOpenConns = n
	}
	if v := os.Getenv("PAD_DB_MAX_IDLE_CONNS"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("config: PAD_DB_MAX_IDLE_CONNS is not a valid integer: %w", err)
		}
		cfg.Storage.DBMaxIdleConns = n
	}
	if v := os.Getenv("PAD_DB_CONN_MAX_LIFETIME"); v != "" {
		cfg.Storage.DBConnMaxLifetime = v
	}
	if v := os.Getenv("PAD_AZURE_STORAGE_ACCOUNT"); v != "" {
		cfg.Storage.AzureStorageAccount = v
	}
	if v := os.Getenv("PAD_AZURE_STORAGE_CONTAINER"); v != "" {
		cfg.Storage.AzureStorageContainer = v
	}
	// Runtime tuning parameters
	if v := os.Getenv("PAD_RATE_LIMIT_GENERAL_RPS"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.Runtime.RateLimitGeneralRPS = f
		}
	}
	if v := os.Getenv("PAD_RATE_LIMIT_GENERAL_BURST"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.Runtime.RateLimitGeneralBurst = f
		}
	}
	if v := os.Getenv("PAD_RATE_LIMIT_AUTH_RPS"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.Runtime.RateLimitAuthRPS = f
		}
	}
	if v := os.Getenv("PAD_RATE_LIMIT_AUTH_BURST"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.Runtime.RateLimitAuthBurst = f
		}
	}
	if v := os.Getenv("PAD_RATE_LIMIT_EXPENSIVE_RPS"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.Runtime.RateLimitExpensiveRPS = f
		}
	}
	if v := os.Getenv("PAD_RATE_LIMIT_EXPENSIVE_BURST"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.Runtime.RateLimitExpensiveBurst = f
		}
	}
	if v := os.Getenv("PAD_RATE_LIMIT_CHAT_RPS"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.Runtime.RateLimitChatRPS = f
		}
	}
	if v := os.Getenv("PAD_RATE_LIMIT_CHAT_BURST"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.Runtime.RateLimitChatBurst = f
		}
	}
	if v := os.Getenv("PAD_RATE_LIMIT_UPLOAD_RPS"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.Runtime.RateLimitUploadRPS = f
		}
	}
	if v := os.Getenv("PAD_RATE_LIMIT_UPLOAD_BURST"); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			cfg.Runtime.RateLimitUploadBurst = f
		}
	}
	if v := os.Getenv("PAD_CB_FAILURES"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Runtime.CircuitBreakerFailures = n
		}
	}
	if v := os.Getenv("PAD_CB_OPEN_DURATION"); v != "" {
		cfg.Runtime.CircuitBreakerOpenDur = v
	}
	if v := os.Getenv("PAD_RETRY_MAX_ATTEMPTS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			cfg.Runtime.RetryMaxAttempts = n
		}
	}
	if v := os.Getenv("PAD_RETRY_BASE_DELAY"); v != "" {
		cfg.Runtime.RetryBaseDelay = v
	}
	if v := os.Getenv("PAD_OTLP_ENDPOINT"); v != "" {
		cfg.Runtime.OTLPEndpoint = v
	}
	if v := os.Getenv("PAD_REQUEST_TIMEOUT"); v != "" {
		cfg.Runtime.RequestTimeout = v
	}
	return nil
}

// minSecretLength is the minimum acceptable JWT signing secret length (bytes)
// for HMAC-SHA256 in a multi-user cloud deployment.
const minSecretLength = 32

// knownWeakSecrets are placeholder/example values that must never be used as a
// real signing secret in cloud mode.
var knownWeakSecrets = map[string]bool{
	"change-me-in-production":         true,
	"change-me":                       true,
	"change-me-to-a-long-random-secret": true,
	"changeme":                        true,
	"secret":                          true,
	"password":                        true,
	"test":                            true,
}

// isWeakSecret reports whether a secret is too short or a known placeholder.
func isWeakSecret(s string) bool {
	if len(s) < minSecretLength {
		return true
	}
	return knownWeakSecrets[strings.ToLower(strings.TrimSpace(s))]
}

// Validate checks that the configuration is internally consistent.
func Validate(cfg *Config) error {
	if cfg.Mode != ModeLocal && cfg.Mode != ModeCloud {
		return fmt.Errorf("config: unknown deployment mode %q", cfg.Mode)
	}
	if cfg.Auth.Enabled && cfg.Auth.Secret == "" {
		return errors.New("config: auth.secret is required when auth.enabled is true")
	}
	// In cloud (multi-user) mode the JWT secret protects every account, so reject
	// short or placeholder secrets that would make token forgery trivial.
	if cfg.Mode == ModeCloud && cfg.Auth.Enabled && isWeakSecret(cfg.Auth.Secret) {
		return fmt.Errorf("config: auth.secret must be at least %d characters and not a known placeholder in cloud mode (set PAD_AUTH_SECRET to a long random value)", minSecretLength)
	}
	// A wildcard CORS origin would let any site make credentialed requests.
	// (An empty list is fine: the SPA is served same-origin by the backend.)
	for _, o := range cfg.Server.AllowedOrigins {
		if strings.TrimSpace(o) == "*" {
			return errors.New("config: wildcard '*' is not permitted in server.allowed_origins; list explicit origins")
		}
	}
	if cfg.Storage.Backend == StorageDatabase && cfg.Storage.DatabaseURL == "" {
		return errors.New("config: storage.database_url is required when storage.backend is database")
	}
	// Azure Blob Storage must be fully configured or not at all.
	if (cfg.Storage.AzureStorageAccount != "" || cfg.Storage.AzureStorageContainer != "") &&
		(cfg.Storage.AzureStorageAccount == "" || cfg.Storage.AzureStorageContainer == "") {
		return errors.New("config: PAD_AZURE_STORAGE_ACCOUNT and PAD_AZURE_STORAGE_CONTAINER must both be set to enable blob storage")
	}
	// SSO must be fully configured or not at all — a partial config (e.g. issuer
	// without redirect URL) silently failing at first login is worse than a
	// startup error.
	sso := cfg.Auth.SSO
	if (sso.IssuerURL != "" || sso.ClientID != "" || sso.RedirectURL != "") && !sso.Enabled() {
		return errors.New("config: PAD_SSO_ISSUER, PAD_SSO_CLIENT_ID, and PAD_SSO_REDIRECT_URL must all be set to enable SSO (client secret is optional for PKCE-only public clients)")
	}
	if sso.Enabled() && !cfg.Auth.Enabled {
		return errors.New("config: SSO requires PAD_AUTH_ENABLED=true")
	}
	if err := validateTrustedProxies(cfg.Server.TrustedProxies); err != nil {
		return err
	}
	if err := validateTLSConfig(cfg); err != nil {
		return err
	}
	return nil
}

// validateTLSConfig enforces the plaintext-credentials safety net:
//
//   - If TLSCert/TLSKey are partially set (one without the other), that's a
//     misconfiguration — refuse.
//   - In cloud mode with auth enabled, the operator must explicitly choose
//     one of: serve TLS directly (TLSCert+TLSKey), or declare a trusted
//     reverse proxy in front (BehindProxy=true). Otherwise the binary on
//     port 80 would broadcast JWTs and passwords in plaintext. The flag
//     pair is the smallest "I know what I'm doing" surface that prevents
//     the most common accidental misdeploy.
func validateTLSConfig(cfg *Config) error {
	hasCert := strings.TrimSpace(cfg.Server.TLSCert) != ""
	hasKey := strings.TrimSpace(cfg.Server.TLSKey) != ""
	if hasCert != hasKey {
		return errors.New("config: server.tls_cert and server.tls_key must be set together (or both empty)")
	}
	if cfg.Mode == ModeCloud && cfg.Auth.Enabled && !hasCert && !cfg.Server.BehindProxy {
		return errors.New("config: cloud-mode auth requires TLS — set PAD_TLS_CERT/PAD_TLS_KEY to serve HTTPS directly, or PAD_BEHIND_PROXY=true if a TLS-terminating reverse proxy is in front. Without one, JWTs and passwords would be sent in plaintext.")
	}
	return nil
}

// validateTrustedProxies enforces that each entry in PAD_TRUSTED_PROXIES is
// either a parseable IP or a parseable CIDR, and that no entry is a wildcard
// that would effectively disable rate-limit isolation. The rate limiter only
// honors X-Forwarded-For when the request's RemoteAddr matches a trusted
// proxy, so a misconfigured allowlist of "*" or "0.0.0.0/0" would silently
// let any client spoof its IP.
func validateTrustedProxies(entries []string) error {
	for _, raw := range entries {
		e := strings.TrimSpace(raw)
		if e == "" {
			continue
		}
		// Reject wildcards explicitly — operators sometimes set "*" expecting
		// "trust everything"; the current code would silently treat that as a
		// literal string that matches no real IP, which masks the misconfig.
		if e == "*" {
			return fmt.Errorf("config: trusted_proxies entry %q is a wildcard; list explicit IPs or CIDRs (use private ranges like 10.0.0.0/8 if needed)", raw)
		}
		if strings.Contains(e, "/") {
			_, prefix, err := net.ParseCIDR(e)
			if err != nil {
				return fmt.Errorf("config: trusted_proxies entry %q is not a valid CIDR: %w", raw, err)
			}
			// Block "trust the entire internet" patterns.
			ones, bits := prefix.Mask.Size()
			if ones == 0 && bits > 0 {
				return fmt.Errorf("config: trusted_proxies entry %q matches all addresses; list narrower ranges", raw)
			}
			continue
		}
		if net.ParseIP(e) == nil {
			return fmt.Errorf("config: trusted_proxies entry %q is not a valid IP address or CIDR", raw)
		}
	}
	return nil
}
