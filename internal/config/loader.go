package config

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
	"strings"
	"time"
)

// parseIntOrError parses a base-10 integer env var, returning a clear error
// (including the var name and value) on failure. Used for numeric env vars that
// previously used the `if err == nil` silent-swallow pattern, which left the
// default in place with no feedback on an operator typo.
func parseIntOrError(envVar, val string) (int, error) {
	n, err := strconv.Atoi(val)
	if err != nil {
		return 0, fmt.Errorf("config: %s=%q is not a valid integer: %w", envVar, val, err)
	}
	return n, nil
}

// parseFloatOrError parses a float64 env var with the same fail-fast contract.
func parseFloatOrError(envVar, val string) (float64, error) {
	f, err := strconv.ParseFloat(val, 64)
	if err != nil {
		return 0, fmt.Errorf("config: %s=%q is not a valid number: %w", envVar, val, err)
	}
	return f, nil
}

// parseDurationOrError validates a duration-string env var (e.g. "30s", "5m",
// "1h") at load time rather than at runtime consumption. Returns the original
// string (the fields are stored as strings consumed later by time.ParseDuration)
// so callers can keep their existing storage type while gaining fail-fast
// validation.
func parseDurationOrError(envVar, val string) (string, error) {
	if _, err := time.ParseDuration(val); err != nil {
		return "", fmt.Errorf("config: %s=%q is not a valid duration (use e.g. \"30s\", \"5m\", \"1h\"): %w", envVar, val, err)
	}
	return val, nil
}

// LoadRaw reads configuration from the given JSON file without validating.
// The caller is responsible for calling Validate on the returned Config.
func LoadRaw(path string) (*Config, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- config path is operator-supplied, not request input
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
func LoadFromEnvRaw() (*Config, error) {
	cfg := Default()
	if err := applyEnvVars(cfg); err != nil {
		return nil, err
	}
	return cfg, nil
}

// Load reads configuration from the given JSON file.
// If the file does not exist it returns Default() without an error.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path) // #nosec G304 -- config path is operator-supplied, not request input
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
//	PAD_ENCRYPTION_KEY         at-rest encryption key (keystore); falls back to PAD_AUTH_SECRET
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

// envBinding maps one PAD_* environment variable to a setter that parses its
// value and assigns it into Config. The typed constructors below (strEnv,
// intEnv, floatEnv, durEnv, boolEnv) build these, so each variable is one
// declarative table row instead of a hand-written if-block.
type envBinding struct {
	key   string
	apply func(*Config, string) error
}

func strEnv(key string, set func(*Config, string)) envBinding {
	return envBinding{key, func(c *Config, v string) error { set(c, v); return nil }}
}

func intEnv(key string, set func(*Config, int)) envBinding {
	return envBinding{key, func(c *Config, v string) error {
		n, err := parseIntOrError(key, v)
		if err != nil {
			return err
		}
		set(c, n)
		return nil
	}}
}

func floatEnv(key string, set func(*Config, float64)) envBinding {
	return envBinding{key, func(c *Config, v string) error {
		f, err := parseFloatOrError(key, v)
		if err != nil {
			return err
		}
		set(c, f)
		return nil
	}}
}

// durEnv validates a duration string but stores it as a string (the Config
// fields are consumed later by time.ParseDuration; see parseDurationOrError).
func durEnv(key string, set func(*Config, string)) envBinding {
	return envBinding{key, func(c *Config, v string) error {
		d, err := parseDurationOrError(key, v)
		if err != nil {
			return err
		}
		set(c, d)
		return nil
	}}
}

func boolEnv(key string, set func(*Config, bool)) envBinding {
	return envBinding{key, func(c *Config, v string) error {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return fmt.Errorf("%s=%q: %w (use true/false, 1/0, or TRUE/FALSE)", key, v, err)
		}
		set(c, b)
		return nil
	}}
}

// envBindings is the declarative table of every simple one-variable→one-field
// binding. Variables needing bespoke handling (list-splitting, the PowerPlatform
// block, PAD_STORAGE normalisation, the PAD_AUDIT_RETENTION_DAYS >= 0 guard) are
// applied separately in applyEnvVars.
var envBindings = []envBinding{
	// Server / mode
	strEnv("PAD_MODE", func(c *Config, v string) { c.Mode = DeploymentMode(v) }),
	strEnv("PAD_HOST", func(c *Config, v string) { c.Server.Host = v }),
	strEnv("PAD_STATIC_DIR", func(c *Config, v string) { c.Server.StaticDir = v }),
	strEnv("PAD_KEYVAULT_URL", func(c *Config, v string) { c.Server.KeyVaultURL = v }),
	strEnv("PAD_CUSTOM_RULES", func(c *Config, v string) { c.Server.CustomRulesPath = v }),
	intEnv("PAD_PORT", func(c *Config, n int) { c.Server.Port = n }),
	strEnv("PAD_METRICS_TOKEN", func(c *Config, v string) { c.Server.MetricsToken = v }),
	strEnv("PAD_TLS_CERT", func(c *Config, v string) { c.Server.TLSCert = v }),
	strEnv("PAD_TLS_KEY", func(c *Config, v string) { c.Server.TLSKey = v }),
	boolEnv("PAD_BEHIND_PROXY", func(c *Config, b bool) { c.Server.BehindProxy = b }),

	// Governance
	durEnv("PAD_SCAN_INTERVAL", func(c *Config, d string) { c.Governance.ScanInterval = d }),
	strEnv("PAD_NOTIFY_WEBHOOK_URL", func(c *Config, v string) { c.Governance.NotifyWebhookURL = v }),
	strEnv("PAD_NOTIFY_WEBHOOK_SECRET", func(c *Config, v string) { c.Governance.NotifyWebhookSecret = v }),
	strEnv("PAD_NOTIFY_TEAMS_URL", func(c *Config, v string) { c.Governance.NotifyTeamsURL = v }),
	strEnv("PAD_NOTIFY_SLACK_URL", func(c *Config, v string) { c.Governance.NotifySlackURL = v }),
	strEnv("PAD_NOTIFY_SLACK_SECRET", func(c *Config, v string) { c.Governance.NotifySlackSecret = v }),
	strEnv("PAD_CI_WEBHOOK_SECRET", func(c *Config, v string) { c.Governance.InboundWebhookSecret = v }),
	strEnv("PAD_NOTIFY_EMAIL_TO", func(c *Config, v string) { c.Governance.NotifyEmailTo = v }),
	strEnv("PAD_NOTIFY_JIRA_BASE_URL", func(c *Config, v string) { c.Governance.NotifyJiraBaseURL = v }),
	strEnv("PAD_NOTIFY_JIRA_EMAIL", func(c *Config, v string) { c.Governance.NotifyJiraEmail = v }),
	strEnv("PAD_NOTIFY_JIRA_API_TOKEN", func(c *Config, v string) { c.Governance.NotifyJiraAPIToken = v }),
	strEnv("PAD_NOTIFY_JIRA_PROJECT", func(c *Config, v string) { c.Governance.NotifyJiraProject = v }),
	durEnv("PAD_RETENTION_PURGE_INTERVAL", func(c *Config, d string) { c.Governance.RetentionPurgeInterval = d }),

	// Email
	strEnv("PAD_SMTP_HOST", func(c *Config, v string) { c.Email.SMTPHost = v }),
	intEnv("PAD_SMTP_PORT", func(c *Config, n int) { c.Email.SMTPPort = n }),
	strEnv("PAD_SMTP_USERNAME", func(c *Config, v string) { c.Email.Username = v }),
	strEnv("PAD_SMTP_PASSWORD", func(c *Config, v string) { c.Email.Password = v }),
	strEnv("PAD_EMAIL_FROM", func(c *Config, v string) { c.Email.From = v }),
	strEnv("PAD_APP_BASE_URL", func(c *Config, v string) { c.Email.AppBaseURL = v }),

	// Redis
	strEnv("PAD_REDIS_URL", func(c *Config, v string) { c.Redis.URL = v }),
	intEnv("PAD_REDIS_POOL_SIZE", func(c *Config, v int) { c.Redis.PoolSize = v }),
	intEnv("PAD_REDIS_MIN_IDLE_CONNS", func(c *Config, v int) { c.Redis.MinIdleConns = v }),

	// Storage
	strEnv("PAD_DATA_DIR", func(c *Config, v string) { c.Storage.DataDir = v }),
	strEnv("PAD_DATABASE_URL", func(c *Config, v string) { c.Storage.DatabaseURL = v }),
	strEnv("PAD_DB_REQUIRE_SSL", func(c *Config, v string) { c.Storage.DBRequireSSL = v }),
	strEnv("PAD_AZURE_STORAGE_ACCOUNT", func(c *Config, v string) { c.Storage.AzureStorageAccount = v }),
	strEnv("PAD_AZURE_STORAGE_CONTAINER", func(c *Config, v string) { c.Storage.AzureStorageContainer = v }),
	strEnv("PAD_AZURE_BLOB_CONNECTION_STRING", func(c *Config, v string) { c.Storage.AzureBlobConnectionString = v }),
	// PostgreSQL connection pool tuning — right-size the pool for the Azure
	// Database for PostgreSQL SKU without a code change.
	intEnv("PAD_DB_MAX_OPEN_CONNS", func(c *Config, n int) { c.Storage.DBMaxOpenConns = n }),
	intEnv("PAD_DB_MAX_IDLE_CONNS", func(c *Config, n int) { c.Storage.DBMaxIdleConns = n }),
	durEnv("PAD_DB_CONN_MAX_LIFETIME", func(c *Config, d string) { c.Storage.DBConnMaxLifetime = d }),
	durEnv("PAD_DB_CONN_MAX_IDLE_TIME", func(c *Config, d string) { c.Storage.DBConnMaxIdleTime = d }),
	// Knowledge-base embedding dimension. Sets the contract for which chunks are
	// pgvector-searchable; mismatched embeddings are excluded from the vector
	// index rather than corrupting similarity search.
	intEnv("PAD_EMBEDDING_DIM", func(c *Config, n int) { c.Storage.EmbeddingDim = n }),
	// Audit overflow spill queue. Empty → temp dir (zero-config); "off" disables.
	strEnv("PAD_AUDIT_SPILL_DIR", func(c *Config, v string) { c.Governance.AuditSpillDir = v }),

	// Auth / SSO
	boolEnv("PAD_AUTH_ENABLED", func(c *Config, b bool) { c.Auth.Enabled = b }),
	strEnv("PAD_AUTH_SECRET", func(c *Config, v string) { c.Auth.Secret = v }),
	strEnv("PAD_ENCRYPTION_KEY", func(c *Config, v string) { c.Auth.EncryptionKey = v }),
	strEnv("PAD_SSO_ISSUER", func(c *Config, v string) { c.Auth.SSO.IssuerURL = v }),
	strEnv("PAD_SSO_CLIENT_ID", func(c *Config, v string) { c.Auth.SSO.ClientID = v }),
	strEnv("PAD_SSO_CLIENT_SECRET", func(c *Config, v string) { c.Auth.SSO.ClientSecret = v }),
	strEnv("PAD_SSO_REDIRECT_URL", func(c *Config, v string) { c.Auth.SSO.RedirectURL = v }),
	strEnv("PAD_SSO_PROVIDER_NAME", func(c *Config, v string) { c.Auth.SSO.ProviderName = v }),

	// Logging
	strEnv("PAD_LOG_LEVEL", func(c *Config, v string) { c.Log.Level = v }),

	// Runtime tuning — rate limits
	floatEnv("PAD_RATE_LIMIT_GENERAL_RPS", func(c *Config, f float64) { c.Runtime.RateLimitGeneralRPS = f }),
	floatEnv("PAD_RATE_LIMIT_GENERAL_BURST", func(c *Config, f float64) { c.Runtime.RateLimitGeneralBurst = f }),
	floatEnv("PAD_RATE_LIMIT_AUTH_RPS", func(c *Config, f float64) { c.Runtime.RateLimitAuthRPS = f }),
	floatEnv("PAD_RATE_LIMIT_AUTH_BURST", func(c *Config, f float64) { c.Runtime.RateLimitAuthBurst = f }),
	floatEnv("PAD_RATE_LIMIT_EXPENSIVE_RPS", func(c *Config, f float64) { c.Runtime.RateLimitExpensiveRPS = f }),
	floatEnv("PAD_RATE_LIMIT_EXPENSIVE_BURST", func(c *Config, f float64) { c.Runtime.RateLimitExpensiveBurst = f }),
	floatEnv("PAD_RATE_LIMIT_CHAT_RPS", func(c *Config, f float64) { c.Runtime.RateLimitChatRPS = f }),
	floatEnv("PAD_RATE_LIMIT_CHAT_BURST", func(c *Config, f float64) { c.Runtime.RateLimitChatBurst = f }),
	floatEnv("PAD_RATE_LIMIT_UPLOAD_RPS", func(c *Config, f float64) { c.Runtime.RateLimitUploadRPS = f }),
	floatEnv("PAD_RATE_LIMIT_UPLOAD_BURST", func(c *Config, f float64) { c.Runtime.RateLimitUploadBurst = f }),
	floatEnv("PAD_RATE_LIMIT_PER_USER_RPS", func(c *Config, f float64) { c.Runtime.RateLimitPerUserRPS = f }),
	floatEnv("PAD_RATE_LIMIT_PER_USER_BURST", func(c *Config, f float64) { c.Runtime.RateLimitPerUserBurst = f }),

	// Runtime tuning — resilience / observability
	intEnv("PAD_CB_FAILURES", func(c *Config, n int) { c.Runtime.CircuitBreakerFailures = n }),
	durEnv("PAD_CB_OPEN_DURATION", func(c *Config, d string) { c.Runtime.CircuitBreakerOpenDur = d }),
	intEnv("PAD_RETRY_MAX_ATTEMPTS", func(c *Config, n int) { c.Runtime.RetryMaxAttempts = n }),
	durEnv("PAD_RETRY_BASE_DELAY", func(c *Config, d string) { c.Runtime.RetryBaseDelay = d }),
	strEnv("PAD_OTLP_ENDPOINT", func(c *Config, v string) { c.Runtime.OTLPEndpoint = v }),
	durEnv("PAD_REQUEST_TIMEOUT", func(c *Config, d string) { c.Runtime.RequestTimeout = d }),

	// Feature flags — env-sourced, read-only product gates.
	boolEnv("PAD_FEATURE_DISABLE_SIGNUP", func(c *Config, b bool) { c.Features.DisableSignUp = b }),
}

// applyEnvVars reads PAD_* environment variables into cfg.
// It returns an error only for syntactically invalid numeric/duration/bool
// values; all other validation is deferred to Validate().
func applyEnvVars(cfg *Config) error {
	for _, b := range envBindings {
		if v := os.Getenv(b.key); v != "" {
			if err := b.apply(cfg, v); err != nil {
				return err
			}
		}
	}

	// Comma-separated lists: appended (not replaced) after trimming blanks.
	applyListEnv("PAD_ALLOWED_ORIGINS", &cfg.Server.AllowedOrigins)
	applyListEnv("PAD_TRUSTED_PROXIES", &cfg.Server.TrustedProxies)

	if err := applyStorageBackendEnv(cfg); err != nil {
		return err
	}
	if err := applyAuditRetentionEnv(cfg); err != nil {
		return err
	}
	return applyPowerPlatformEnv(cfg)
}

// applyListEnv appends the trimmed, non-empty comma-separated entries of key
// onto dst (leaving dst untouched when key is unset).
func applyListEnv(key string, dst *[]string) {
	v := os.Getenv(key)
	if v == "" {
		return
	}
	for _, item := range strings.Split(v, ",") {
		if trimmed := strings.TrimSpace(item); trimmed != "" {
			*dst = append(*dst, trimmed)
		}
	}
}

// applyStorageBackendEnv normalises PAD_STORAGE case/whitespace so a capitalised
// "Database" or "LOCAL" isn't silently treated as an unknown backend — without
// this, the cloud-mode storage check passes vacuously (the literal "Database" !=
// "database") and the deployment runs with NO backend.
func applyStorageBackendEnv(cfg *Config) error {
	v := os.Getenv("PAD_STORAGE")
	if v == "" {
		return nil
	}
	switch norm := strings.ToLower(strings.TrimSpace(v)); norm {
	case "local", "database":
		cfg.Storage.Backend = StorageBackend(norm)
		return nil
	default:
		return fmt.Errorf("PAD_STORAGE=%q: unknown backend (use \"local\" or \"database\")", v)
	}
}

// applyAuditRetentionEnv parses PAD_AUDIT_RETENTION_DAYS and rejects negatives
// (0 = keep audit history indefinitely).
func applyAuditRetentionEnv(cfg *Config) error {
	v := os.Getenv("PAD_AUDIT_RETENTION_DAYS")
	if v == "" {
		return nil
	}
	days, err := parseIntOrError("PAD_AUDIT_RETENTION_DAYS", v)
	if err != nil {
		return err
	}
	if days < 0 {
		return fmt.Errorf("config: PAD_AUDIT_RETENTION_DAYS=%q must be >= 0", v)
	}
	cfg.Governance.AuditRetentionDays = days
	return nil
}

// applyPowerPlatformEnv wires the optional Power Platform connector (desktop-flow
// ingestion; EXPERIMENTAL — see config.PowerPlatformConfig). The bare
// assignments intentionally overwrite defaults even with empty values, matching
// the original behaviour. The core auth/client fields plus DataverseURL and
// IngestInterval must be set to enable the periodic pull.
func applyPowerPlatformEnv(cfg *Config) error {
	cfg.PowerPlatform.TenantID = os.Getenv("PAD_PP_TENANT_ID")
	cfg.PowerPlatform.ClientID = os.Getenv("PAD_PP_CLIENT_ID")
	cfg.PowerPlatform.DataverseURL = os.Getenv("PAD_PP_DATAVERSE_URL")
	cfg.PowerPlatform.Scope = os.Getenv("PAD_PP_SCOPE")
	if cfg.PowerPlatform.Scope == "" {
		cfg.PowerPlatform.Scope = "https://api.powerplatform.com/.default"
		// The default scope grants access to the ENTIRE Power Platform API
		// surface, not just the intended Dataverse environment. A leaked token
		// could access other environments, Power Apps, etc. Set PAD_PP_SCOPE
		// to an environment-specific scope for least-privilege.
		slog.Warn("PAD_PP_SCOPE unset — using broad /.default scope; set PAD_PP_SCOPE to an environment-specific scope for least-privilege")
	}
	if v := os.Getenv("PAD_PP_INGEST_INTERVAL"); v != "" {
		d, err := parseDurationOrError("PAD_PP_INGEST_INTERVAL", v)
		if err != nil {
			return err
		}
		cfg.PowerPlatform.IngestInterval = d
	}
	cfg.PowerPlatform.OwnerUserID = os.Getenv("PAD_PP_OWNER_USER")
	cfg.PowerPlatform.OwnerOrgID = os.Getenv("PAD_PP_OWNER_ORG")
	return nil
}

// minSecretLength is the minimum acceptable JWT signing secret length (bytes)
// for HMAC-SHA256 in a multi-user cloud deployment.
const minSecretLength = 32

// knownWeakSecrets are placeholder/example values that must never be used as a
// real signing secret in cloud mode. Most are caught by the length floor
// (minSecretLength), but several common documented placeholders are ≥32 chars
// (copy-pasted from tutorials, READMEs, jwt.io, boilerplate) and would slip
// past length alone — the blocklist rejects them explicitly.
var knownWeakSecrets = map[string]bool{
	// Generic placeholders / boilerplate.
	"change-me-in-production":           true,
	"change-me":                         true,
	"change-me-to-a-long-random-secret": true,
	"changeme":                          true,
	"secret":                            true,
	"password":                          true,
	"test":                              true,
	// Long (≥32-char) placeholders that pass the length floor — the highest-
	// risk category because they look "configured" to a casual reviewer.
	"please-change-this-secret-in-production":   true,
	"your-super-secret-jwt-key-change-me":       true,
	"do-not-use-this-secret-in-production":      true,
	"this-is-a-placeholder-secret-replace-it":   true,
	"replace-with-a-real-secret-key-please":     true,
	"your-256-bit-secret-here-replace-it":       true,
	"test-secret-key-for-development-only":      true,
	"dev-secret-not-for-production-use-please":  true,
	"super-secret-key-please-change-before-use": true,
	// Repeated-character defaults emitted by some generators/scripts.
	strings.Repeat("x", 36): true,
	strings.Repeat("0", 36): true,
	strings.Repeat("a", 36): true,
}

// isLowEntropy catches pathologically weak secrets that are long enough to pass
// the length floor but have near-zero actual entropy — a single repeated byte
// ("aaaa…", "0000…") or a short tile (2–4 bytes) repeated to fill the string
// ("ababab…", "abcdabcd…"). A genuinely random secret is astronomically
// unlikely to match either shape, so this has no realistic false-positive risk.
func isLowEntropy(s string) bool {
	if len(s) < minSecretLength {
		return false
	}
	allSame := true
	for i := 1; i < len(s); i++ {
		if s[i] != s[0] {
			allSame = false
			break
		}
	}
	if allSame {
		return true
	}
	for tile := 2; tile <= 4; tile++ {
		if isTiled(s, tile) {
			return true
		}
	}
	return false
}

// isTiled reports whether s is exactly the n-byte prefix repeated.
func isTiled(s string, n int) bool {
	if len(s) < minSecretLength || len(s)%n != 0 {
		return false
	}
	tile := s[:n]
	for i := n; i < len(s); i += n {
		if s[i:i+n] != tile {
			return false
		}
	}
	return true
}

// isWeakSecret reports whether a secret is too short, a known placeholder, or
// low-entropy (repeated-character / tiled).
func isWeakSecret(s string) bool {
	s = strings.TrimSpace(s)
	if len(s) < minSecretLength {
		return true
	}
	if knownWeakSecrets[strings.ToLower(s)] {
		return true
	}
	return isLowEntropy(s)
}

// Validate checks that the configuration is internally consistent.
func Validate(cfg *Config) error {
	if cfg.Mode != ModeLocal && cfg.Mode != ModeCloud {
		return fmt.Errorf("config: unknown deployment mode %q", cfg.Mode)
	}
	// Cloud (multi-tenant) mode MUST enforce authentication. The entire authz
	// stack is gated on Auth.Enabled: RequireRole returns true unconditionally,
	// the JWT middleware falls back to a single shared bearer token, and the
	// RLS transaction wrapper becomes a no-op. An operator who sets
	// PAD_MODE=cloud but forgets PAD_AUTH_ENABLED=true would otherwise deploy a
	// system where every tenant's data is reachable with no real auth. Fail
	// closed instead of silently running unauthenticated.
	if cfg.Mode == ModeCloud && !cfg.Auth.Enabled {
		return errors.New("config: cloud mode requires PAD_AUTH_ENABLED=true (multi-tenant deployments must enforce authentication)")
	}
	if cfg.Auth.Enabled && cfg.Auth.Secret == "" {
		return errors.New("config: auth.secret is required when auth.enabled is true")
	}
	// In cloud (multi-user) mode the JWT secret protects every account, so reject
	// short or placeholder secrets that would make token forgery trivial.
	if cfg.Mode == ModeCloud && cfg.Auth.Enabled && isWeakSecret(cfg.Auth.Secret) {
		return fmt.Errorf("config: auth.secret must be at least %d characters and not a known placeholder in cloud mode (set PAD_AUTH_SECRET to a long random value)", minSecretLength)
	}
	// The dedicated encryption key protects stored provider credentials at rest
	// (AES-256-GCM). Apply the same strength gate as the JWT secret so a weak
	// PAD_ENCRYPTION_KEY can't make a DB leak trivially brute-forceable. When
	// unset, the auth secret is used as a backward-compat fallback (warned in
	// main.go), so only enforce when an explicit key is provided.
	if cfg.Mode == ModeCloud && cfg.Auth.EncryptionKey != "" && isWeakSecret(cfg.Auth.EncryptionKey) {
		return fmt.Errorf("config: auth.encryption_key must be at least %d characters and not a known placeholder in cloud mode (set PAD_ENCRYPTION_KEY to a long random value)", minSecretLength)
	}
	// A wildcard CORS origin would let any site make credentialed requests.
	// (An empty list is fine: the SPA is served same-origin by the backend.)
	for _, o := range cfg.Server.AllowedOrigins {
		if strings.TrimSpace(o) == "*" {
			return errors.New("config: wildcard '*' is not permitted in server.allowed_origins; list explicit origins")
		}
	}
	if cfg.Mode == ModeCloud && cfg.Storage.Backend != StorageDatabase {
		return errors.New("config: cloud mode requires storage.backend=database (set PAD_STORAGE=database)")
	}
	if cfg.Storage.Backend == StorageDatabase && cfg.Storage.DatabaseURL == "" {
		return errors.New("config: storage.database_url is required when storage.backend is database")
	}
	// Azure Blob Storage: a container is required, plus exactly one auth source —
	// either an account name (Managed Identity, the prod default) or a connection
	// string (emulator / non-MI). A bare account or bare container is a misconfig.
	if cfg.Storage.AzureBlobConnectionString != "" {
		if cfg.Storage.AzureStorageContainer == "" {
			return errors.New("config: PAD_AZURE_STORAGE_CONTAINER must be set when PAD_AZURE_BLOB_CONNECTION_STRING is set")
		}
	} else if (cfg.Storage.AzureStorageAccount != "" || cfg.Storage.AzureStorageContainer != "") &&
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
		return errors.New("config: cloud-mode auth requires TLS — set PAD_TLS_CERT/PAD_TLS_KEY to serve HTTPS directly, or PAD_BEHIND_PROXY=true if a TLS-terminating reverse proxy is in front (without one, JWTs and passwords would be sent in plaintext)")
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
