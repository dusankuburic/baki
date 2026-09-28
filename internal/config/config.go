package config

import "pad-core/models"

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

// DefaultEmbeddingDim is the knowledge-base vector width when
// PAD_EMBEDDING_DIM is unset (OpenAI text-embedding-3-small's native width).
// The storage layer's pgvector contract and the width the AI factory asks
// embedding models for must agree, so both read it from here.
const DefaultEmbeddingDim = 1536

// EmbeddingProviderIDs lists the provider ids PAD_EMBEDDING_PROVIDER accepts:
// the embedding-capable providers. The ai package cannot be imported here (it
// imports config); an ai test keeps the two lists in sync.
var EmbeddingProviderIDs = []string{"openai", "gemini", "glm", "github-models", "azure-foundry"}

// EffectiveEmbeddingDim is the knowledge-base vector width in force:
// EmbeddingDim, or DefaultEmbeddingDim when unset.
func (s StorageConfig) EffectiveEmbeddingDim() int {
	if s.EmbeddingDim > 0 {
		return s.EmbeddingDim
	}
	return DefaultEmbeddingDim
}

// Config holds the complete application configuration
type Config struct {
	Mode       DeploymentMode
	Server     ServerConfig
	Storage    StorageConfig
	Auth       AuthConfig
	Runtime    RuntimeConfig
	Governance GovernanceConfig
	Email      EmailConfig
	Redis      RedisConfig
	Features   FeatureFlags
	Log        models.LogConfig
}

// FeatureFlags gates optional product behaviour at runtime. Flags are env-sourced
// (PAD_FEATURE_*) and read-only — an operator sets them at deploy time to lock
// down a tenant (e.g. DisableSignUp for a closed deployment). Exposed publicly
// via GET /api/system/features so the frontend can hide disabled affordances.
type FeatureFlags struct {
	// DisableSignUp blocks new account self-registration. Existing users can
	// still log in; admins provision accounts out-of-band. Useful for
	// single-tenant / enterprise deployments that don't allow open signup.
	DisableSignUp bool `json:"disableSignUp"`
}

// RedisConfig configures the optional shared backplane. When URL is empty (the
// default) the app keeps all rate-limit / presence / chat-resume state
// in-process — correct for single-replica. Setting URL switches those subsystems
// to a Redis-backed shared store so they work consistently across replicas.
// The backplane is opt-in: nothing changes
// unless URL is set.
type RedisConfig struct {
	// URL is a redis://[user:pass@]host:port/[db] connection string. Empty =
	// backplane disabled (in-memory, single-replica).
	URL string
	// PoolSize overrides go-redis's default (10 × GOMAXPROCS). 0 = default.
	PoolSize int
	// MinIdleConns sets the minimum number of idle connections. 0 = default.
	MinIdleConns int
}

// EmailConfig configures transactional email (password reset, email
// verification, org invites). It is fully optional: when SMTPHost/From are
// unset, the app falls back to a log-only mailer (links are written to the
// server log) so non-email deployments keep working. AppBaseURL is the public
// origin used to build links in emails, e.g. https://app.example.com.
type EmailConfig struct {
	SMTPHost   string
	SMTPPort   int
	Username   string
	Password   string
	From       string
	AppBaseURL string
}

// Enabled reports whether SMTP delivery is configured.
func (e EmailConfig) Enabled() bool {
	return e.SMTPHost != "" && e.From != ""
}

// GovernanceConfig configures the continuous-governance loop: the periodic flow
// scanner and its outbound alert channels. All zero values disable the feature
// (scanning is opt-in), so it has no effect unless an interval and at least one
// channel are set. Cloud mode only — the scanner needs a storage backend.
type GovernanceConfig struct {
	// ScanInterval is how often to re-scan stored flows for drift/regressions
	// (a duration string like "1h"; empty or non-positive disables scanning).
	ScanInterval string
	// NotifyWebhookURL receives raw JSON alert events (generic webhook).
	NotifyWebhookURL string
	// NotifyWebhookSecret is the HMAC-SHA256 key for webhook payload signing
	// (X-Baki-Signature header). Empty = no signature.
	NotifyWebhookSecret string
	// NotifyTeamsURL is a Microsoft Teams incoming-webhook URL (MessageCard).
	NotifyTeamsURL string
	// NotifySlackURL is a Slack incoming-webhook URL. The Slack payload format
	// is also compatible with Discord, Mattermost, and Teams (Slack-compatible
	// connector). For backward compatibility, PAD_WEBHOOK_URL is treated as an
	// alias for this channel when NotifySlackURL is unset.
	NotifySlackURL string
	// NotifySlackSecret is an optional HMAC key for Slack payload signing.
	NotifySlackSecret string
	// InboundWebhookSecret is the HMAC key used to verify inbound CI webhooks
	// (POST /api/integrations/ci). The CI runner signs the request body with
	// HMAC-SHA256 and sends X-Baki-Signature: sha256=<hex>. Empty disables the
	// endpoint (it returns 503). Separate from the outbound signing secrets so
	// rotating one doesn't break the other.
	InboundWebhookSecret string
	// NotifyEmailTo is the recipient address for governance alert emails (an ops
	// alias or team list). Empty disables the email channel even with SMTP set.
	NotifyEmailTo string
	// Jira: when all four are set, a Jira Cloud issue is created per alert.
	// JiraBaseURL is the cloud base (e.g. https://acme.atlassian.net).
	NotifyJiraBaseURL string
	// JiraEmail + JiraAPIToken form the Basic-auth credential (API token).
	NotifyJiraEmail    string
	NotifyJiraAPIToken string
	// JiraProject is the target project key (e.g. "PAD").
	NotifyJiraProject string
	// RetentionPurgeInterval is how often to purge expired tokens/invites and
	// aged-out audit rows (a duration string like "24h"; empty or non-positive
	// disables the periodic purge). Cloud mode only — the purge needs a DB.
	RetentionPurgeInterval string
	// AuditRetentionDays is how long audit_events are kept before the purge
	// deletes them (0 = keep audit history indefinitely; only expired tokens
	// and invites are purged).
	AuditRetentionDays int
	// AuditSpillDir is the directory for the audit-event on-disk spill queue
	// used when the in-memory pool overflows. Empty → os.TempDir() (zero-config,
	// but a pod restart loses in-flight spilled events). Set to a mounted volume
	// for cross-restart durability. "off" disables the spill queue entirely.
	AuditSpillDir string
}

// ServerConfig holds HTTP server settings
type ServerConfig struct {
	Host           string
	Port           int
	AllowedOrigins []string // CORS / WebSocket origin allowlist (cloud mode)
	TrustedProxies []string // IPs of trusted reverse proxies for rate limiting
	StaticDir      string   // Directory for static frontend assets
	// MetricsToken, when set, additionally requires a matching bearer token on
	// /metrics and /debug/pprof (defense-in-depth against a misconfigured NSG
	// exposing the endpoints publicly). Empty = IP-allowlist only (default).
	//
	// The IP allowlist alone is NOT sufficient behind a reverse proxy: it keys
	// off r.RemoteAddr, which is the proxy's own (private) address for every
	// external request. Set this whenever BehindProxy is true.
	MetricsToken string
	// LocalConfigEndpoint serves the session token from GET /api/local-config in
	// local mode. Default false, and it must stay that way for the desktop app.
	//
	// That route is public (no token required) and, in local mode, CORS reflects
	// ANY loopback Origin — so with it enabled, any page served from any
	// localhost port (a Vite dev server, a Jupyter notebook, a locally-served
	// HTML report) can read the desktop app's session token cross-origin by
	// scanning ports. With that token it reaches every other /api route,
	// including load-path (arbitrary file READ — deliberately unconfined,
	// because the desktop client legitimately opens files anywhere) and the
	// export endpoints (arbitrary file WRITE, since they honour a
	// caller-supplied path outside cloud mode).
	//
	// The Tauri webview does not need it: TauriAdapter gets the token through
	// invoke("get_backend_config"), which reads the 0600 session file. The
	// endpoint exists only for browser-based local development, which is the
	// one context that should switch it on.
	LocalConfigEndpoint bool
	// PprofEnabled registers the /debug/pprof/* handlers. Default false.
	//
	// This is opt-in rather than always-on because a profile is a different
	// class of exposure from a metrics scrape: /debug/pprof/heap dumps live
	// process memory (JWT signing secret, decrypted provider API keys, flow
	// content, chat history) and /debug/pprof/profile burns CPU on demand. The
	// private-IP guard those handlers shared with /metrics is vacuous behind a
	// proxy, so they were world-reachable on every ACA/reverse-proxy
	// deployment. Enabling this REQUIRES MetricsToken (see Validate).
	PprofEnabled bool
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
	// CustomRulesPath is the path to a JSON file of user-defined analyzer
	// rules (see core/analyzer/custom_rules.go). Empty = no custom rules.
	// Set via PAD_CUSTOM_RULES.
	CustomRulesPath string
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
	DBConnMaxIdleTime string // duration string, e.g. "5m"
	// DBStatementTimeout bounds every statement (and lock wait) server-side:
	// PAD_DB_STATEMENT_TIMEOUT, e.g. "30s". Empty ⇒ the backend default
	// (30s); "0s" disables for a maintenance run.
	DBStatementTimeout string

	// DBRequireSSL controls whether the Postgres DSN must enforce TLS.
	// It is a raw tri-state string parsed from PAD_DB_REQUIRE_SSL:
	// "true"/"1"/"yes" ⇒ require, "false"/"0"/"no" ⇒ allow plaintext, empty ⇒
	// fall back to the deployment default (required in cloud mode, optional in
	// local mode). Resolved to a bool in main.provideStorageBackend where the
	// deployment mode is known.
	DBRequireSSL string

	// EmbeddingDim is the vector dimension the knowledge-base embeddings use.
	// It gates which chunks are eligible for pgvector server-side search: a
	// chunk whose embedding width differs from this is kept in the JSONB
	// fallback column but skipped for the vector index (and ranked Go-side when
	// pgvector is unavailable), mirroring the "re-index after changing the
	// embedding provider" contract. 0 ⇒ DefaultEmbeddingDim (see
	// EffectiveEmbeddingDim).
	EmbeddingDim int

	// EmbeddingProvider pins the provider that embeds the org knowledge base
	// for the whole deployment (PAD_EMBEDDING_PROVIDER); it wins over the
	// settings' AI.EmbeddingProvider. Empty ⇒ the settings value, else
	// "openai", with a fallback to the caller's other embedding-capable keys.
	// "azure-foundry" is the only way to embed with Azure AI Foundry: a
	// user's Azure document names a deployment of THEIR choosing, so it never
	// joins the shared corpus by default (a second model at the same width
	// would silently split the org's retrieval). Validate accepts only
	// EmbeddingProviderIDs.
	EmbeddingProvider string

	// Azure Blob Storage settings (optional). Used when Backend == StorageDatabase.
	AzureStorageAccount   string
	AzureStorageContainer string
	// AzureBlobConnectionString, when set, builds the blob client from a
	// connection string instead of account name + Managed Identity. Intended for
	// the Azurite emulator and non-MI/local scenarios; Managed Identity (via
	// AzureStorageAccount) remains the production default.
	AzureBlobConnectionString string
}

type RuntimeConfig struct {
	RateLimitGeneralRPS     float64
	RateLimitGeneralBurst   float64
	RateLimitAuthRPS        float64
	RateLimitAuthBurst      float64
	RateLimitExpensiveRPS   float64
	RateLimitExpensiveBurst float64
	RateLimitChatRPS        float64
	RateLimitChatBurst      float64
	RateLimitUploadRPS      float64
	RateLimitUploadBurst    float64
	// RateLimitPerUser caps one authenticated user's total WRITE throughput
	// (POST/PUT/PATCH/DELETE) across all endpoints — an additional ceiling on
	// top of the per-group per-IP buckets, so a single account can't rotate
	// across endpoints to evade a per-group cap. Reads (GET) bypass it (the
	// per-IP limiter still covers them). Cloud mode only; local mode is
	// single-user so it would self-DoS.
	RateLimitPerUserRPS    float64
	RateLimitPerUserBurst  float64
	CircuitBreakerFailures int
	CircuitBreakerOpenDur  string
	RetryMaxAttempts       int
	RetryBaseDelay         string
	OTLPEndpoint           string
	RequestTimeout         string
	// AzureFoundryAllowedHosts extends the hosts a user may configure as an
	// Azure AI Foundry endpoint beyond Microsoft's own suffixes
	// (*.openai.azure.com, *.services.ai.azure.com, …): exact hosts or
	// "*.suffix" globs, for API Management fronts and private DNS zones
	// (PAD_AZURE_FOUNDRY_ALLOWED_HOSTS).
	AzureFoundryAllowedHosts []string
	// AIAllowPrivateEgress disables the cloud-mode dial-time guard that
	// refuses AI endpoints resolving to private/loopback addresses
	// (PAD_AI_ALLOW_PRIVATE_EGRESS). Only for private-link resources, whose
	// hosts resolve to RFC 1918 addresses by design; the endpoint allow-list
	// still applies.
	AIAllowPrivateEgress bool
}

func DefaultRuntimeConfig() RuntimeConfig {
	return RuntimeConfig{
		RateLimitGeneralRPS:     1.0,
		RateLimitGeneralBurst:   20,
		RateLimitAuthRPS:        5.0 / 60.0,
		RateLimitAuthBurst:      5,
		RateLimitExpensiveRPS:   2,
		RateLimitExpensiveBurst: 5,
		RateLimitChatRPS:        3,
		RateLimitChatBurst:      10,
		RateLimitUploadRPS:      1,
		RateLimitUploadBurst:    3,
		// Per-user default: generous enough that a legit user doing one thing at
		// a time (already bounded by the tighter per-group per-IP caps) never
		// hits it, but caps a runaway account's total write volume.
		RateLimitPerUserRPS:    10,
		RateLimitPerUserBurst:  20,
		CircuitBreakerFailures: 5,
		CircuitBreakerOpenDur:  "30s",
		RetryMaxAttempts:       3,
		RetryBaseDelay:         "500ms",
		OTLPEndpoint:           "",
		RequestTimeout:         "30s",
	}
}

type AuthConfig struct {
	// Enabled controls whether auth middleware is enforced
	Enabled bool
	// Secret is the JWT signing key (required when Enabled == true)
	Secret string
	// EncryptionKey is the dedicated key for at-rest encryption (e.g. the
	// database keystore AES-256-GCM key). Separate from Secret so a JWT-key
	// leak doesn't compromise stored provider keys, and so rotating the JWT
	// key doesn't brick encrypted secrets. Falls back to Secret when unset
	// (backward compatibility, emits a deprecation warning).
	EncryptionKey string
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
			Port: 0, // 0 = OS-assigned ephemeral port
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
		Governance: GovernanceConfig{
			// On by default (cloud mode only; see initRetentionPurge). Off, the
			// expired-token and share-link tables grew forever, and nothing
			// told an operator the purge existed — its env var was documented
			// nowhere. "0s" disables. Age-based purges (audit, history, usage
			// metrics, alerts) additionally need PAD_AUDIT_RETENTION_DAYS.
			RetentionPurgeInterval: "24h",
		},
	}
}
