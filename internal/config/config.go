package config

import (
	"time"

	"pad-analyzer/internal/platform/secret"
	"pad-core/models"
)

// DeploymentMode indicates how the application is deployed
type DeploymentMode string

const (
	ModeLocal DeploymentMode = "local"
	ModeCloud DeploymentMode = "cloud"
)

// RoutePolicyMode says which route policy decides each request (PAD_ROUTE_POLICY; ADR 0006).
type RoutePolicyMode string

const (
	// RoutePolicyLegacy: the middleware's path tables decide; the declared policies are not consulted.
	RoutePolicyLegacy RoutePolicyMode = "legacy"
	// RoutePolicyShadow: the path tables decide, and the policy declared on the route is computed alongside;
	// every disagreement is counted (pad_route_policy_mismatch_total) and logged once. The default.
	RoutePolicyShadow RoutePolicyMode = "shadow"
	// RoutePolicyDeclarative: the declared policy decides; the path tables are not consulted.
	RoutePolicyDeclarative RoutePolicyMode = "declarative"
)

// StorageBackend indicates which storage implementation to use
type StorageBackend string

const (
	StorageLocal    StorageBackend = "local"
	StorageDatabase StorageBackend = "database"
)

// DefaultEmbeddingDim is the knowledge-base vector width when
// PAD_EMBEDDING_DIM is unset (text-embedding-3-small's native width); the
// storage layer's pgvector contract and the AI factory's width must agree.
const DefaultEmbeddingDim = 1536

// DefaultKnowledgeLeaseCap is how many org knowledge-base leases a replica holds at once when
// PAD_KNOWLEDGE_LEASE_CAP is unset; rag.DefaultLeaseCap is the same number for a service built without config.
const DefaultKnowledgeLeaseCap = 4

// DBIdleTxTimeoutMargin is how much longer than the request timeout a transaction may sit idle before the server ends
// it, when PAD_DB_IDLE_TX_TIMEOUT is unset (EffectiveDBIdleTxTimeout). A request transaction begins after its request
// does, so an idle stretch longer than the request timeout belongs to a handler already past its deadline; the
// margin is room for what may legitimately idle beyond that: a handler persisting its partial work after the deadline
// (15 s), a post-commit hook's own scope, and off a request a store method's own transaction waiting on one blob
// operation (45 s, storage's blobOpTimeout, which a test there keeps below this).
const DBIdleTxTimeoutMargin = time.Minute

// EmbeddingProviderIDs lists the provider ids PAD_EMBEDDING_PROVIDER accepts:
// the embedding-capable providers. The ai package cannot be imported here (it
// imports config); an ai test keeps the lists in sync.
var EmbeddingProviderIDs = []string{"openai", "gemini", "glm", "github-models", "azure-foundry"}

// EffectiveEmbeddingDim is the knowledge-base vector width in force:
// EmbeddingDim, or DefaultEmbeddingDim when unset.
func (s StorageConfig) EffectiveEmbeddingDim() int {
	if s.EmbeddingDim > 0 {
		return s.EmbeddingDim
	}
	return DefaultEmbeddingDim
}

// EffectiveDBIdleTxTimeout is the idle_in_transaction_session_timeout in force: Storage.DBIdleTxTimeout when it is
// set (0 = off), else the request timeout plus DBIdleTxTimeoutMargin, derived from whichever request timeout the
// configuration holds.
func (c *Config) EffectiveDBIdleTxTimeout() time.Duration {
	if d, set := c.Storage.DBIdleTxTimeout.Get(); set {
		return d
	}
	return c.Runtime.RequestTimeout.Std() + DBIdleTxTimeoutMargin
}

// DefaultRAGDeadline is how long a chat turn waits for its knowledge-base guidelines when PAD_RAG_DEADLINE_MS is unset:
// the turn's first chunk waits on the lookup (corpus count, embedder, query embedding, search), so it stays short.
const DefaultRAGDeadline = 800 * time.Millisecond

// MaxRAGDeadlineMS is the longest PAD_RAG_DEADLINE_MS Validate accepts: past it a slow embedding provider would hold
// every org turn's first chunk for longer than the user waits for an answer to start.
const MaxRAGDeadlineMS = 10000

// EffectiveRAGDeadline is a chat turn's wait for its knowledge-base guidelines: RAGDeadlineMS, or DefaultRAGDeadline
// when unset.
func (r RuntimeConfig) EffectiveRAGDeadline() time.Duration {
	if r.RAGDeadlineMS > 0 {
		return time.Duration(r.RAGDeadlineMS) * time.Millisecond
	}
	return DefaultRAGDeadline
}

// EffectiveKnowledgeLeaseCap is the per-replica cap of org knowledge-base leases in force: KnowledgeLeaseCap, or
// DefaultKnowledgeLeaseCap when unset.
func (s StorageConfig) EffectiveKnowledgeLeaseCap() int {
	if s.KnowledgeLeaseCap > 0 {
		return s.KnowledgeLeaseCap
	}
	return DefaultKnowledgeLeaseCap
}

// Config holds the complete application configuration.
//
// Every credential, and every value that carries one (a DSN's password, a webhook URL's token), is a secret.Value:
// it prints, marshals and logs as "[redacted]", and only Reveal() at the boundary that needs the plain text (the
// driver, the signer, the keystore) reads it. A config file holds it as a plain JSON string under the same name.
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
// down a tenant. Exposed via GET /api/system/features so the frontend can hide
// disabled affordances.
type FeatureFlags struct {
	// DisableSignUp blocks new account self-registration; existing users can
	// still log in, admins provision accounts out-of-band (closed deployments).
	DisableSignUp bool `json:"disableSignUp"`
}

// RedisConfig configures the optional shared backplane. When URL is empty (the
// default) the app keeps all rate-limit / presence / chat-resume state
// in-process — correct for single-replica. Setting URL switches those
// subsystems to a Redis-backed shared store so they work across replicas.
type RedisConfig struct {
	// URL is a redis://[user:pass@]host:port/[db] connection string. Empty =
	// backplane disabled (in-memory, single-replica).
	URL secret.Value
	// PoolSize overrides go-redis's default (10 × GOMAXPROCS). 0 = default.
	PoolSize int
	// MinIdleConns sets the minimum number of idle connections. 0 = default.
	MinIdleConns int
}

// EmailConfig configures transactional email (password reset, email
// verification, org invites). Fully optional: when SMTPHost/From are unset the
// app falls back to a log-only mailer so non-email deployments keep working.
// AppBaseURL is the public origin used to build links in emails.
type EmailConfig struct {
	SMTPHost   string
	SMTPPort   int
	Username   string
	Password   secret.Value
	From       string
	AppBaseURL string
	// MaxConnections caps the SMTP sessions open to the relay at once (PAD_SMTP_MAX_CONNECTIONS); 0 leaves only the
	// mail runner's bound of 32.
	MaxConnections int
}

// Enabled reports whether SMTP delivery is configured.
func (e EmailConfig) Enabled() bool {
	return e.SMTPHost != "" && e.From != ""
}

// GovernanceConfig configures the continuous-governance loop: the periodic flow
// scanner and its outbound alert channels. All zero values disable the feature
// (scanning is opt-in). Cloud mode only — the scanner needs a storage backend.
type GovernanceConfig struct {
	// ScanInterval is how often to re-scan stored flows for drift/regressions
	// (PAD_SCAN_INTERVAL, e.g. "1h"); 0, the default, disables scanning.
	ScanInterval Duration
	// NotifyWebhookURL receives raw JSON alert events (generic webhook).
	NotifyWebhookURL secret.Value
	// NotifyWebhookSecret is the HMAC-SHA256 key for webhook payload signing
	// (X-Baki-Signature header); empty = no signature.
	NotifyWebhookSecret secret.Value
	// NotifyTeamsURL is a Microsoft Teams incoming-webhook URL (MessageCard). Microsoft is retiring the Office 365
	// connectors behind it; NotifyTeamsWorkflowURL is the replacement.
	NotifyTeamsURL secret.Value
	// NotifyTeamsWorkflowURL is a Microsoft Teams Workflows webhook URL (an Adaptive Card message).
	NotifyTeamsWorkflowURL secret.Value
	// NotifySlackURL is a Slack incoming-webhook URL. The Slack payload format
	// is also compatible with Discord, Mattermost, and Teams. PAD_WEBHOOK_URL
	// is treated as an alias for this channel when NotifySlackURL is unset.
	NotifySlackURL secret.Value
	// NotifySlackSecret is an optional HMAC key for Slack payload signing.
	NotifySlackSecret secret.Value
	// NotifyAllowedHosts restricts the hosts an organisation's channels may point at (PAD_NOTIFY_ALLOWED_HOSTS):
	// exact hosts or "*.suffix" globs, checked when a channel is saved and again when it is delivered to; empty
	// allows any public host. The deployment's own channels (PAD_NOTIFY_*) are not restricted. Globs over public
	// suffixes and multi-tenant zones are refused at boot.
	NotifyAllowedHosts []string
	// NotifyAllowedHostsVisible (PAD_NOTIFY_ALLOWED_HOSTS_VISIBLE) shows NotifyAllowedHosts to organisation admins in
	// the channel form. Off by default: the list may name other customers' hosts, so admins learn only that one
	// applies. Turn it on when the list names only shared service hosts (hooks.slack.com, *.logic.azure.com).
	NotifyAllowedHostsVisible bool
	// InboundWebhookSecret is the deployment-wide HMAC key of the inbound CI
	// webhook (POST /api/integrations/ci): the CI runner signs
	// "<X-Baki-Timestamp>." + body with HMAC-SHA256 and sends X-Baki-Signature:
	// sha256=<hex>. Separate from the outbound signing secrets so rotating one
	// doesn't break the other. With accounts, organisations mint their own CI
	// credentials (ADR 0019) and this secret is deprecated; without accounts,
	// empty disables the endpoint (503).
	InboundWebhookSecret secret.Value
	// InboundWebhookAllowNoTimestamp (PAD_CI_WEBHOOK_ALLOW_NO_TIMESTAMP=1) lets
	// InboundWebhookSecret verify the legacy signature over the body alone,
	// without X-Baki-Timestamp, so a captured request replays forever.
	// Deprecated; organisation CI credentials never accept that form.
	InboundWebhookAllowNoTimestamp bool
	// NotifyEmailTo is the recipient address for governance alert emails (an ops
	// alias or team list); empty disables the email channel even with SMTP set.
	NotifyEmailTo string
	// Jira: when all four are set, a Jira Cloud issue is created per alert.
	// JiraBaseURL is the cloud base (e.g. https://acme.atlassian.net).
	NotifyJiraBaseURL string
	// JiraEmail + JiraAPIToken form the Basic-auth credential (API token).
	NotifyJiraEmail    string
	NotifyJiraAPIToken secret.Value
	// JiraProject is the target project key (e.g. "PAD").
	NotifyJiraProject string
	// RetentionPurgeInterval is how often to purge expired tokens/invites and
	// aged-out audit rows (PAD_RETENTION_PURGE_INTERVAL, default 24h; "0s"
	// disables). With accounts on the database only — the purge needs a DB.
	RetentionPurgeInterval Duration
	// AuditRetentionDays is the purge window, in days, of the audit trail, analysis history, usage, alerts and
	// rule notices (PAD_AUDIT_RETENTION_DAYS; 0 = keep them indefinitely). DefaultCloudAuditRetentionDays in the
	// cloud profile, 0 for local and desktop; an explicit 0 keeps everything there too.
	AuditRetentionDays int
	// AuditSpillDir is the directory for the audit-event on-disk spill queue
	// used when the in-memory pool overflows. Empty → os.TempDir() (zero-config,
	// but a pod restart loses in-flight spilled events); a mounted volume gives
	// cross-restart durability; "off" disables the spill queue entirely. Each
	// replica spills to audit-spill-<instance>.jsonl; on a shared volume a
	// replica replays the files of replicas that are gone (internal/audit).
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
	// The IP allowlist alone is NOT sufficient behind a reverse proxy: it keys
	// off r.RemoteAddr, which is the proxy's own (private) address for every
	// external request — set this whenever BehindProxy is true.
	MetricsToken secret.Value
	// LocalConfigEndpoint serves the session token from GET /api/local-config in
	// local mode. Default false, and it must stay that way for the desktop app.
	//
	// That route is public (no token required) and, in local mode, CORS reflects
	// ANY loopback Origin — so with it enabled, any page served from any
	// localhost port (a Vite dev server, a Jupyter notebook) can read the
	// desktop app's session token cross-origin by scanning ports, and with it
	// reach the load-path (arbitrary file READ — deliberately unconfined for
	// the desktop client) and the export endpoints (arbitrary file WRITE via a
	// caller-supplied path outside cloud mode). The Tauri webview does not
	// need it: TauriAdapter gets the token through invoke("get_backend_config"),
	// which reads the 0600 session file. Browser-based local development only.
	LocalConfigEndpoint bool
	// PprofEnabled registers the /debug/pprof/* handlers. Default false.
	//
	// Opt-in rather than always-on because a profile is a different class of
	// exposure from a metrics scrape: /debug/pprof/heap dumps live process
	// memory (JWT signing secret, decrypted provider API keys, flow content)
	// and /debug/pprof/profile burns CPU on demand; the private-IP guard is
	// vacuous behind a proxy. Enabling this REQUIRES MetricsToken (see Validate).
	PprofEnabled bool
	// TLSCert and TLSKey are paths to PEM-encoded cert/key files; when both
	// are set the server uses ListenAndServeTLS directly. Leave empty when
	// terminating TLS at a reverse proxy and set BehindProxy=true instead.
	TLSCert string
	TLSKey  string
	// BehindProxy declares that a trusted TLS-terminating reverse proxy is
	// in front — the operator's "I know what I'm doing" flag. Without it (and
	// without TLSCert/TLSKey), a cloud-mode deployment with auth enabled
	// refuses to start, to prevent accidental plaintext credentials.
	BehindProxy bool
	// CustomRulesPath is the path to a JSON file of user-defined analyzer
	// rules (see core/analyzer/custom_rules.go); empty = none. PAD_CUSTOM_RULES.
	CustomRulesPath string
	// RoutePolicy says whether the legacy path tables or the policies declared
	// on the routes decide auth, scopes, rate limits, the RLS transaction and
	// deadlines (PAD_ROUTE_POLICY; default shadow).
	RoutePolicy RoutePolicyMode
}

// StorageConfig holds storage backend settings
type StorageConfig struct {
	// DataDir is the root directory for local file storage
	DataDir string
	Backend StorageBackend
	// DatabaseURL is used when Backend == StorageDatabase
	DatabaseURL secret.Value

	// Connection pool settings for the PostgreSQL backend: PAD_DB_MAX_OPEN_CONNS
	// and PAD_DB_MAX_IDLE_CONNS (0 = the backend's default, see
	// database.DefaultConfig), PAD_DB_CONN_MAX_LIFETIME (default 1h, "0s" =
	// connections are never retired for age) and PAD_DB_CONN_MAX_IDLE_TIME
	// (default 5m; 0 is the backend's 5m too).
	DBMaxOpenConns    int
	DBMaxIdleConns    int
	DBConnMaxLifetime Duration
	DBConnMaxIdleTime Duration
	// DBStatementTimeout bounds every statement (and lock wait) server-side:
	// PAD_DB_STATEMENT_TIMEOUT, default 30s; "0s" disables for a maintenance run.
	DBStatementTimeout Duration
	// DBIdleTxTimeout ends a session whose transaction sits idle for longer (idle_in_transaction_session_timeout):
	// PAD_DB_IDLE_TX_TIMEOUT, "0s" disables. Unset, it is the request timeout plus DBIdleTxTimeoutMargin; read it
	// through EffectiveDBIdleTxTimeout. Validate requires a set value to be longer than the request timeout.
	DBIdleTxTimeout OptionalDuration
	// MaxReplicas is how many replicas share the database server at most
	// (PAD_MAX_REPLICAS, which infra/main.bicep sets); 0 = unknown, counted as
	// one. Only the boot check of the pool against max_connections reads it.
	MaxReplicas int
	// DBServerMaxConns is the server's max_connections as the deployment knows
	// it (PAD_DB_SERVER_MAX_CONNS); 0 = unknown. The pool check uses it when
	// the server's own settings cannot be read.
	DBServerMaxConns int

	// DBRequireSSL controls whether the Postgres DSN must enforce TLS. It is
	// a raw tri-state string parsed from PAD_DB_REQUIRE_SSL: "true"/"1"/"yes"
	// ⇒ require, "false"/"0"/"no" ⇒ allow plaintext, empty ⇒ the deployment
	// default (required in cloud, optional in local). Resolved to a bool in
	// main.provideStorageBackend where the deployment mode is known.
	DBRequireSSL string
	// DBAllowBypassRLS (PAD_DB_ALLOW_BYPASSRLS) lets the cloud profile start on a database role that bypasses
	// Row-Level Security (a superuser or BYPASSRLS), with the warning the other profiles log. Without it the cloud
	// refuses to start on such a role: every tenant policy would be inert.
	DBAllowBypassRLS bool

	// EmbeddingDim is the vector dimension the knowledge-base embeddings use.
	// It gates which chunks are eligible for pgvector server-side search: a
	// chunk whose embedding width differs is kept in the JSONB fallback column
	// but skipped for the vector index (and ranked Go-side when pgvector is
	// unavailable), mirroring the "re-index after changing the embedding
	// provider" contract. 0 ⇒ DefaultEmbeddingDim (EffectiveEmbeddingDim).
	EmbeddingDim int

	// EmbeddingProvider pins the provider that embeds the org knowledge base
	// for the whole deployment (PAD_EMBEDDING_PROVIDER); it wins over the
	// settings' AI.EmbeddingProvider. Empty ⇒ the settings value, else
	// "openai", with a fallback to the caller's other embedding-capable keys.
	// "azure-foundry" is the only way to embed with Azure AI Foundry: a user's
	// Azure document names a deployment of THEIR choosing, so it never joins
	// the shared corpus by default (a second model at the same width would
	// silently split the org's retrieval). Validate accepts only
	// EmbeddingProviderIDs.
	EmbeddingProvider string
	// KnowledgeLeaseCap is how many org knowledge-base leases (an upload or a re-index of one org, held across
	// every replica) one replica holds at once: PAD_KNOWLEDGE_LEASE_CAP. Each pins a pooled database connection for
	// the operation, which lasts seconds to minutes; the operation beyond the cap answers 409 KNOWLEDGE_BUSY. 0 ⇒
	// DefaultKnowledgeLeaseCap (EffectiveKnowledgeLeaseCap).
	KnowledgeLeaseCap int

	// Azure Blob Storage settings (optional). Used when Backend == StorageDatabase.
	AzureStorageAccount   string
	AzureStorageContainer string
	// AzureBlobConnectionString, when set, builds the blob client from a
	// connection string instead of account name + Managed Identity — for the
	// Azurite emulator and non-MI/local scenarios; Managed Identity remains
	// the production default.
	AzureBlobConnectionString secret.Value
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
	// RateLimitChannelTest is the notification channel test's own bucket: each call makes the server POST to an
	// admin-chosen public URL for up to 15 s.
	RateLimitChannelTestRPS   float64
	RateLimitChannelTestBurst float64
	// RateLimitAvatar is the avatar upload's own bucket (each call decodes, downscales and re-encodes an image of up
	// to 16 M pixels), apart from the flow uploads' so neither refuses the other.
	RateLimitAvatarRPS   float64
	RateLimitAvatarBurst float64
	// RateLimitControl is the bucket of the chat stream commands (Stop, an approval decision), per address: apart
	// from the general one, so a client whose other requests drained that bucket can still stop a billed stream.
	RateLimitControlRPS   float64
	RateLimitControlBurst float64
	// RateLimitEmbed is the knowledge base's bulk embedding bucket (upload and its continuations, re-index), per
	// address: each request can buy thousands of chunk embeddings, so it is a cost bucket of its own, not the
	// general one (R-security-c22).
	RateLimitEmbedRPS   float64
	RateLimitEmbedBurst float64
	// RateLimitPerUser caps one authenticated user's total WRITE throughput
	// (POST/PUT/PATCH/DELETE) across all endpoints — an additional ceiling on
	// the per-group per-IP buckets, so an account can't rotate across
	// endpoints to evade a per-group cap. Reads (GET) bypass it (the per-IP
	// limiter still covers them). Cloud mode only; local mode is single-user.
	RateLimitPerUserRPS    float64
	RateLimitPerUserBurst  float64
	CircuitBreakerFailures int
	CircuitBreakerOpenDur  Duration
	RetryMaxAttempts       int
	RetryBaseDelay         Duration
	OTLPEndpoint           string
	// RequestTimeout is the deadline of an ordinary request (PAD_REQUEST_TIMEOUT); the streams and the
	// knowledge-base bulk routes have their own. Validate requires it positive.
	RequestTimeout Duration
	// InstanceID names this process among the replicas: logs, traces, the Postgres application_name, a job
	// lock's holder. PAD_INSTANCE_ID, else the replica name Azure Container Apps sets, else the host name, else a
	// random id (Load resolves it; never empty after Load).
	InstanceID string
	// ShutdownDrainDelay is how long a stopping server answers "draining" (readiness 503, new streams refused)
	// while it still serves, before it ends the open streams, so probes and load balancers move traffic first
	// (PAD_SHUTDOWN_DRAIN_DELAY; ADR 0024). Default 3 s in the cloud profile, 0 for local and desktop; at most 5 s.
	ShutdownDrainDelay Duration
	// RAGDeadlineMS is how long, in milliseconds, a chat turn on an organisation's flow waits for its knowledge-base
	// guidelines before it answers without them (PAD_RAG_DEADLINE_MS; R-observability-5): a lookup past it is counted
	// as pad_chat_rag_lookups_total{outcome="deadline"} and the turn says so in a knowledge notice. 0 =>
	// DefaultRAGDeadline (EffectiveRAGDeadline); Validate refuses a negative value and one above MaxRAGDeadlineMS.
	RAGDeadlineMS int
	// AzureFoundryAllowedHosts extends the hosts a user may configure as an
	// Azure AI Foundry endpoint beyond Microsoft's own suffixes
	// (*.openai.azure.com, *.services.ai.azure.com, …): exact hosts or
	// "*.suffix" globs, for API Management fronts and private DNS zones
	// (PAD_AZURE_FOUNDRY_ALLOWED_HOSTS). Globs over public suffixes and
	// multi-tenant zones are refused at boot.
	AzureFoundryAllowedHosts []string
	// AIAllowPrivateEgress relaxes the cloud-mode dial-time guard for AI
	// endpoints to allow RFC 1918 and IPv6 unique-local addresses
	// (PAD_AI_ALLOW_PRIVATE_EGRESS). Only for private-link resources; loopback,
	// link-local (169.254.169.254) and unspecified addresses stay refused, and
	// the endpoint allow-list still applies.
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
		// Channel tests: six a minute after a burst of three.
		RateLimitChannelTestRPS:   0.1,
		RateLimitChannelTestBurst: 3,
		// Avatar uploads: the upload bucket's rate, which they shared before, so no client loses capacity (several
		// users behind one NAT address share it).
		RateLimitAvatarRPS:   1,
		RateLimitAvatarBurst: 3,
		// Stream commands: a person's Stop and approval clicks never come near it, even with others behind one NAT
		// address; a flood (bogus tokens included) is still refused before authentication.
		RateLimitControlRPS:   2,
		RateLimitControlBurst: 20,
		// Knowledge embedding: one request per 5 s sustained, ten back to back. A paged upload or re-index sends its
		// next request only after one has spent its embedding budget (90 s) or finished, so it is never refused; ten
		// small documents uploaded in a row pass at once.
		RateLimitEmbedRPS:   0.2,
		RateLimitEmbedBurst: 10,
		// Per-user default: generous enough that a legit user doing one thing at
		// a time (already bounded by the tighter per-group per-IP caps) never
		// hits it, but caps a runaway account's total write volume.
		RateLimitPerUserRPS:    10,
		RateLimitPerUserBurst:  20,
		CircuitBreakerFailures: 5,
		CircuitBreakerOpenDur:  Duration(30 * time.Second),
		RetryMaxAttempts:       3,
		RetryBaseDelay:         Duration(500 * time.Millisecond),
		OTLPEndpoint:           "",
		RequestTimeout:         Duration(30 * time.Second),
	}
}

type AuthConfig struct {
	// Enabled controls whether auth middleware is enforced
	Enabled bool
	// Secret is the JWT signing key (required when Enabled == true)
	Secret secret.Value
	// EncryptionKey is the dedicated key for at-rest encryption (e.g. the
	// database keystore AES-256-GCM key). Separate from Secret so a JWT-key
	// leak doesn't compromise stored provider keys, and rotating the JWT key
	// doesn't brick encrypted secrets. Falls back to Secret when unset
	// (backward compatibility, emits a deprecation warning).
	EncryptionKey secret.Value
	// EncryptionKeyPrevious are the keys EncryptionKey replaced, newest first (PAD_ENCRYPTION_KEY_PREVIOUS): they
	// still open what they sealed until the re-seal pass has moved it to EncryptionKey (ADR 0005). Only beside a
	// dedicated EncryptionKey.
	EncryptionKeyPrevious []secret.Value
	// SecretEphemeral is true when Secret was minted for this process because none was configured (only
	// possible with auth disabled: Validate requires a secret when auth is enabled). Such a secret must never
	// key data that outlives the process, such as the database keystore. Only ResolveRuntimeSecrets sets it: a
	// config file cannot (it would make the keystore refuse the real secret beside it).
	SecretEphemeral bool `json:"-"`
	// SSO configures account-level OIDC login (cloud mode only)
	SSO SSOConfig
}

// SSOConfig holds OIDC single-sign-on settings. SSO is enabled when
// IssuerURL, ClientID, and RedirectURL are all set (and auth is enabled).
// Works with any OIDC-compliant IdP via discovery (Entra ID, Google, Okta,
// Keycloak, etc.).
type SSOConfig struct {
	// IssuerURL is the OIDC issuer, e.g.
	// https://login.microsoftonline.com/{tenant}/v2.0 or https://accounts.google.com;
	// discovery is fetched from {IssuerURL}/.well-known/openid-configuration.
	IssuerURL string
	// ClientID is the OAuth2 client ID from the IdP app registration.
	ClientID string
	// ClientSecret is optional — leave empty for public clients using PKCE only.
	ClientSecret secret.Value
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
			Host:        "localhost",
			Port:        0, // 0 = OS-assigned ephemeral port
			RoutePolicy: RoutePolicyShadow,
		},
		Storage: StorageConfig{
			Backend: StorageLocal,
			// The Postgres backend's own defaults (database.DefaultConfig;
			// internal/app's TestStorageConfig_DefaultsAreTheBackends keeps them
			// equal). Held here so an explicit "0s" (off) and unset differ.
			DBConnMaxLifetime:  Duration(time.Hour),
			DBConnMaxIdleTime:  Duration(5 * time.Minute),
			DBStatementTimeout: Duration(30 * time.Second),
		},
		Auth: AuthConfig{
			Enabled: false,
		},
		// Info in every mode unless PAD_LOG_LEVEL or a config file says otherwise.
		Log: models.LogConfig{
			Level: "info",
		},
		Governance: GovernanceConfig{
			// On by default (cloud mode only; see initRetentionPurge). Off, the
			// expired-token and share-link tables grew forever, and nothing
			// told an operator the purge existed. "0s" disables; age-based
			// purges additionally need PAD_AUDIT_RETENTION_DAYS (the cloud
			// profile's default, defaultsFor).
			RetentionPurgeInterval: Duration(24 * time.Hour),
		},
	}
}
