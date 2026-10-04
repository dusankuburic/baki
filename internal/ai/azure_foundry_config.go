package ai

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/egress"
	"pad-core/logger"
)

// Azure AI Foundry is configured per user with a small JSON document instead of a bare API key: the resource
// endpoint, the credential (API key or Entra ID service principal) and optional deployment names / project
// endpoint — the document is what the KeyStore holds under the "azure-foundry" provider id.

const (
	azureFoundryProviderID = "azure-foundry"
	// AzureFoundryProviderID is the provider id (KeyStore key, request field).
	AzureFoundryProviderID = azureFoundryProviderID

	// AzureAuthModeAPIKey authenticates with the resource's API key (header
	// api-key); model inference only — agent routes reject keys.
	AzureAuthModeAPIKey = "api_key"
	// AzureAuthModeEntra authenticates as a Microsoft Entra ID service principal
	// (client-credentials flow, scope https://ai.azure.com/.default); required for agents.
	AzureAuthModeEntra = "entra"
	// AzureAuthModeEntraCLI authenticates as the Azure CLI's signed-in USER (its first-party public client:
	// no app registration or secret). Desktop only — a server-side `az login` would be the operator's identity.
	AzureAuthModeEntraCLI = "entra_cli"

	// azureConfigMaxBytes bounds the stored document: the Windows Credential Manager (desktop keyring) rejects
	// secrets above 2560 bytes; a real document is a few hundred.
	azureConfigMaxBytes = 2048

	// azureMaxDeployments bounds the "additional deployments" list so the document stays under azureConfigMaxBytes.
	azureMaxDeployments = 16

	azureMaxClaudeDeployments = 20
)

// Validation failure codes, returned to the client in the error envelope.
const (
	AzureCfgErrInvalidConfig   = "INVALID_CONFIG"
	AzureCfgErrTooLarge        = "CONFIG_TOO_LARGE"
	AzureCfgErrInvalidEndpoint = "INVALID_ENDPOINT"
	AzureCfgErrHostNotAllowed  = "HOST_NOT_ALLOWED"
	AzureCfgErrInvalidProject  = "INVALID_PROJECT_ENDPOINT"
	AzureCfgErrInvalidAuth     = "INVALID_AUTH"
	AzureCfgErrMissingSecret   = "MISSING_SECRET"
	AzureCfgErrInvalidDeploy   = "INVALID_DEPLOYMENT"
	// AzureCfgErrMissingDeploy: the document names no chat deployment (chatDeployment or deployments) and nothing
	// can list one — only Entra ID with a project endpoint can.
	AzureCfgErrMissingDeploy = "MISSING_DEPLOYMENT"
)

// AzureConfigError is a validation failure with a machine-readable code.
type AzureConfigError struct {
	Code    string
	Message string
}

func (e *AzureConfigError) Error() string { return e.Message }

func azureCfgErr(code, format string, args ...any) error {
	return &AzureConfigError{Code: code, Message: fmt.Sprintf(format, args...)}
}

// AzureFoundryAuth is the credential half of the document.
type AzureFoundryAuth struct {
	Mode string `json:"mode"`
	// Key is the resource API key (AzureAuthModeAPIKey).
	Key string `json:"key,omitempty"`
	// TenantID, ClientID and ClientSecret describe the app registration (AzureAuthModeEntra).
	TenantID     string `json:"tenantId,omitempty"`
	ClientID     string `json:"clientId,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty"`
}

// AzureFoundryConfig is the stored per-user document.
type AzureFoundryConfig struct {
	// Endpoint is the resource endpoint, e.g. https://my-res.openai.azure.com or https://my-res.services.ai.azure.com (no path).
	Endpoint string           `json:"endpoint"`
	Auth     AzureFoundryAuth `json:"auth"`
	// ProjectEndpoint (optional) is the Foundry project endpoint, https://<res>.services.ai.azure.com/api/projects/<project>,
	// which unlocks the project's agents (Entra ID only) and deployment metadata.
	ProjectEndpoint string `json:"projectEndpoint,omitempty"`
	// ChatDeployment is the deployment offered as the default model; EmbeddingDeployment (optional) the
	// knowledge-base one; Deployments (≤ azureMaxDeployments) further chat deployments — an API-key caller's
	// deployments are not listable, so the picker shows exactly these names. Saving requires a chat deployment
	// unless the project listing can supply one (MISSING_DEPLOYMENT).
	ChatDeployment      string   `json:"chatDeployment,omitempty"`
	EmbeddingDeployment string   `json:"embeddingDeployment,omitempty"`
	Deployments         []string `json:"deployments,omitempty"`
	// ClaudeDeployments (≤ azureMaxClaudeDeployments) answer on the Anthropic Messages route
	// ({endpoint}/anthropic/v1/messages), not /openai/v1 (404); offered as "claude:<name>" and also discovered
	// by the project listing (modelPublisher "Anthropic").
	ClaudeDeployments []string `json:"claudeDeployments,omitempty"`
}

// AzureFoundryPublicConfig is the redacted view returned to clients: every field but the secrets, plus whether a secret is stored.
type AzureFoundryPublicConfig struct {
	Configured          bool     `json:"configured"`
	Endpoint            string   `json:"endpoint,omitempty"`
	AuthMode            string   `json:"authMode,omitempty"`
	HasSecret           bool     `json:"hasSecret"`
	TenantID            string   `json:"tenantId,omitempty"`
	ClientID            string   `json:"clientId,omitempty"`
	ProjectEndpoint     string   `json:"projectEndpoint,omitempty"`
	ChatDeployment      string   `json:"chatDeployment,omitempty"`
	EmbeddingDeployment string   `json:"embeddingDeployment,omitempty"`
	Deployments         []string `json:"deployments,omitempty"`
	ClaudeDeployments   []string `json:"claudeDeployments,omitempty"`
}

// Configured reports whether the document names an endpoint (an empty KeyStore value parses to an unconfigured config).
func (c AzureFoundryConfig) Configured() bool { return c.Endpoint != "" }

// HasSecret reports whether the credential for the selected mode is present.
func (c AzureFoundryConfig) HasSecret() bool {
	switch c.Auth.Mode {
	case AzureAuthModeAPIKey:
		return c.Auth.Key != ""
	case AzureAuthModeEntra:
		return c.Auth.ClientSecret != ""
	case AzureAuthModeEntraCLI:
		// No secret of its own: the CLI's signed-in user is the credential.
		return true
	}
	return false
}

func (c AzureFoundryConfig) Public() AzureFoundryPublicConfig {
	if !c.Configured() {
		return AzureFoundryPublicConfig{}
	}
	return AzureFoundryPublicConfig{
		Configured:          true,
		Endpoint:            c.Endpoint,
		AuthMode:            c.Auth.Mode,
		HasSecret:           c.HasSecret(),
		TenantID:            c.Auth.TenantID,
		ClientID:            c.Auth.ClientID,
		ProjectEndpoint:     c.ProjectEndpoint,
		ChatDeployment:      c.ChatDeployment,
		EmbeddingDeployment: c.EmbeddingDeployment,
		Deployments:         append([]string(nil), c.Deployments...),
		ClaudeDeployments:   append([]string(nil), c.ClaudeDeployments...),
	}
}

// chatDeployments is every chat deployment the document names: the default (ChatDeployment) first, then the additional ones.
func (c AzureFoundryConfig) chatDeployments() []string {
	out := make([]string, 0, 1+len(c.Deployments))
	if c.ChatDeployment != "" {
		out = append(out, c.ChatDeployment)
	}
	return append(out, c.Deployments...)
}

// listsProjectDeployments reports whether the project deployments listing is reachable: it needs a project
// endpoint and Entra ID (the project data plane rejects API keys).
func (c AzureFoundryConfig) listsProjectDeployments() bool {
	return c.ProjectEndpoint != "" && (c.Auth.Mode == AzureAuthModeEntra || c.Auth.Mode == AzureAuthModeEntraCLI)
}

// azureEgressBlocksInternal reports whether the operator's runtime policy is the cloud one (the desktop/local
// app installs egress.AllowLoopback): the CLI-sign-in auth mode is refused under it.
func azureEgressBlocksInternal() bool {
	azureRuntime.mu.RLock()
	defer azureRuntime.mu.RUnlock()
	return azureRuntime.configured && azureRuntime.opts.Egress == egress.BlockInternal
}

// Encode serialises the document for the KeyStore, enforcing the size cap.
func (c AzureFoundryConfig) Encode() (string, error) {
	raw, err := json.Marshal(c)
	if err != nil {
		return "", azureCfgErr(AzureCfgErrInvalidConfig, "encode Azure AI Foundry configuration: %v", err)
	}
	if len(raw) > azureConfigMaxBytes {
		return "", azureCfgErr(AzureCfgErrTooLarge, "Azure AI Foundry configuration exceeds %d bytes", azureConfigMaxBytes)
	}
	return string(raw), nil
}

// Fingerprint identifies the credential set without revealing it (a changed key or endpoint must never reuse
// another's cache). It deliberately leaves the deployment names out; the model-listing cache extends it.
func (c AzureFoundryConfig) Fingerprint() string {
	h := sha256.New()
	for _, part := range []string{c.Endpoint, c.ProjectEndpoint, c.Auth.Mode, c.Auth.Key, c.Auth.TenantID, c.Auth.ClientID, c.Auth.ClientSecret} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ParseAzureFoundryConfig decodes the stored document ("" is a valid, unconfigured config). Unknown fields are
// rejected so a document written by a newer build is not silently truncated by an older one.
func ParseAzureFoundryConfig(raw string) (AzureFoundryConfig, error) {
	var cfg AzureFoundryConfig
	if strings.TrimSpace(raw) == "" {
		return cfg, nil
	}
	if len(raw) > azureConfigMaxBytes {
		return cfg, azureCfgErr(AzureCfgErrTooLarge, "Azure AI Foundry configuration exceeds %d bytes", azureConfigMaxBytes)
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return AzureFoundryConfig{}, azureCfgErr(AzureCfgErrInvalidConfig, "Azure AI Foundry configuration is not valid JSON: %v", err)
	}
	cfg.normalize()
	return cfg, nil
}

// Normalize trims every field and canonicalises the endpoints (lower-case host, no trailing slash) so equal
// configurations compare equal. Parse does it; the API layer calls it on a freshly decoded request body.
func (c *AzureFoundryConfig) Normalize() { c.normalize() }

func (c *AzureFoundryConfig) normalize() {
	c.Endpoint = normalizeAzureURL(c.Endpoint)
	c.ProjectEndpoint = normalizeAzureURL(c.ProjectEndpoint)
	c.Auth.Mode = strings.ToLower(strings.TrimSpace(c.Auth.Mode))
	c.Auth.Key = strings.TrimSpace(c.Auth.Key)
	c.Auth.TenantID = strings.ToLower(strings.TrimSpace(c.Auth.TenantID))
	c.Auth.ClientID = strings.ToLower(strings.TrimSpace(c.Auth.ClientID))
	c.Auth.ClientSecret = strings.TrimSpace(c.Auth.ClientSecret)
	c.ChatDeployment = strings.TrimSpace(c.ChatDeployment)
	c.EmbeddingDeployment = strings.TrimSpace(c.EmbeddingDeployment)
	seen := map[string]bool{strings.ToLower(c.ChatDeployment): true, strings.ToLower(c.EmbeddingDeployment): true}
	c.Deployments = normalizeAzureDeployments(c.Deployments, seen)
	// A name already listed as a chat-completions deployment is dropped: one deployment answers on one route.
	c.ClaudeDeployments = normalizeAzureDeployments(c.ClaudeDeployments, seen)
}

// normalizeAzureDeployments trims a list of deployment names, dropping blanks, case-insensitive duplicates and
// the names in seen, keeping first-occurrence order; nil when nothing is left.
func normalizeAzureDeployments(names []string, seen map[string]bool) []string {
	var out []string
	for _, n := range names {
		n = strings.TrimSpace(n)
		if n == "" || seen[strings.ToLower(n)] {
			continue
		}
		seen[strings.ToLower(n)] = true
		out = append(out, n)
	}
	return out
}

func normalizeAzureURL(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return raw // left for Validate to reject with a precise message
	}
	u.Scheme = strings.ToLower(u.Scheme)
	u.Host = strings.ToLower(u.Host)
	u.Path = strings.TrimRight(u.Path, "/")
	// A bare trailing "?" (an address-bar copy): kept, String() re-emits it and every join yields
	// "host?/openai/…" — a 404 on every call after a save that succeeded. A real query string is left for Validate.
	if u.RawQuery == "" {
		u.ForceQuery = false
	}
	// The explicit default port is the default: drop it so equal endpoints compare equal (fingerprint, breaker and memo keys).
	if u.Port() == "443" {
		u.Host = strings.TrimSuffix(u.Host, ":443")
	}
	return u.String()
}

var (
	// azureFoundryHostSuffixes are the DNS suffixes Microsoft documents for Foundry / Azure OpenAI resources in
	// the Azure public cloud. Serverless (MaaS) endpoints (<deployment>.<region>.models.ai.azure.com) are refused
	// (they do not serve /openai/v1); the broader ".ai.azure.com" is not listed — it covers portal/service hosts.
	azureFoundryHostSuffixes = []string{
		".openai.azure.com",
		".services.ai.azure.com",
		".cognitiveservices.azure.com",
	}
	azureDeploymentName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	// azureProjectPath: the project name starts with a letter or digit, so "." and ".." (also as %2e%2e, which
	// url.Parse decodes) can never make the joined project routes climb out of /api/projects/.
	azureProjectPath = regexp.MustCompile(`^/api/projects/[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	azureGUID        = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	// azureTenantDomain is a tenant's verified domain name (contoso.onmicrosoft.com, contoso.com): lower-case
	// DNS labels, at least two. The input is lower-cased by normalize.
	azureTenantDomain = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// azureMultiTenantAuthorities are the Entra ID pseudo-tenants that are not a tenant: a client-credentials sign-in needs the app's home tenant.
var azureMultiTenantAuthorities = map[string]bool{"common": true, "organizations": true, "consumers": true}

// validAzureTenantID accepts a directory (tenant) ID — a GUID — or one of the tenant's domain names
// (contoso.onmicrosoft.com), and refuses the multi-tenant pseudo-tenants and IP literals.
func validAzureTenantID(id string) bool {
	if azureGUID.MatchString(id) {
		return true
	}
	if azureMultiTenantAuthorities[id] || len(id) > 253 || !azureTenantDomain.MatchString(id) {
		return false
	}
	_, err := netip.ParseAddr(id)
	return err != nil
}

// Validate checks the document's shape and the endpoint allow-list. It does no DNS resolution and no network
// I/O (the dial-time guard covers runtime), so it is cheap enough to run on every provider construction.
func (c AzureFoundryConfig) Validate(hosts azureHostPolicy) error {
	if !c.Configured() {
		return azureCfgErr(AzureCfgErrInvalidEndpoint, "the Azure AI Foundry endpoint is required")
	}
	if err := validateAzureEndpoint(c.Endpoint, hosts, false); err != nil {
		return err
	}
	if c.ProjectEndpoint != "" {
		if err := validateAzureEndpoint(c.ProjectEndpoint, hosts, true); err != nil {
			return err
		}
	}
	switch c.Auth.Mode {
	case AzureAuthModeAPIKey:
		if c.Auth.Key == "" {
			return azureCfgErr(AzureCfgErrMissingSecret, "an API key is required for API key authentication")
		}
		if len(c.Auth.Key) > 512 {
			return azureCfgErr(AzureCfgErrInvalidAuth, "the API key is too long")
		}
	case AzureAuthModeEntra:
		if !validAzureTenantID(c.Auth.TenantID) {
			return azureCfgErr(AzureCfgErrInvalidAuth, "the tenant ID must be the directory (tenant) ID GUID or a domain name of the tenant such as contoso.onmicrosoft.com (common, organizations and consumers are not tenants)")
		}
		if !azureGUID.MatchString(c.Auth.ClientID) {
			return azureCfgErr(AzureCfgErrInvalidAuth, "the client ID must be a GUID")
		}
		if c.Auth.ClientSecret == "" {
			return azureCfgErr(AzureCfgErrMissingSecret, "a client secret is required for Microsoft Entra ID authentication")
		}
		if len(c.Auth.ClientSecret) > 512 {
			return azureCfgErr(AzureCfgErrInvalidAuth, "the client secret is too long")
		}
	case AzureAuthModeEntraCLI:
		// The CLI's signed-in user is the credential: an optional tenant, never a client id or secret. Desktop
		// only — a cloud deployment's egress policy refuses the mode outright.
		if azureEgressBlocksInternal() {
			return azureCfgErr(AzureCfgErrInvalidAuth, "the Azure CLI sign-in mode is available in the desktop app only — use a Microsoft Entra ID service principal on a server deployment")
		}
		if c.Auth.TenantID != "" && !validAzureTenantID(c.Auth.TenantID) {
			return azureCfgErr(AzureCfgErrInvalidAuth, "the tenant ID must be the directory (tenant) ID GUID or a domain name of the tenant such as contoso.onmicrosoft.com (common, organizations and consumers are not tenants)")
		}
		if c.Auth.ClientID != "" || c.Auth.ClientSecret != "" {
			return azureCfgErr(AzureCfgErrInvalidAuth, "the Azure CLI sign-in mode takes no client ID or client secret — leave them empty (an optional tenant ID is all it accepts)")
		}
	default:
		return azureCfgErr(AzureCfgErrInvalidAuth, "unknown authentication mode %q (use %q, %q or %q)", c.Auth.Mode, AzureAuthModeAPIKey, AzureAuthModeEntra, AzureAuthModeEntraCLI)
	}
	if len(c.Deployments) > azureMaxDeployments {
		return azureCfgErr(AzureCfgErrInvalidDeploy, "at most %d additional deployments can be listed", azureMaxDeployments)
	}
	if len(c.ClaudeDeployments) > azureMaxClaudeDeployments {
		return azureCfgErr(AzureCfgErrInvalidDeploy, "at most %d Claude deployments can be listed", azureMaxClaudeDeployments)
	}
	names := append([]string{c.ChatDeployment, c.EmbeddingDeployment}, c.Deployments...)
	for _, d := range append(names, c.ClaudeDeployments...) {
		if d != "" && !azureDeploymentName.MatchString(d) {
			return azureCfgErr(AzureCfgErrInvalidDeploy, "deployment name %q may only contain letters, digits, '.', '_' and '-' (max 64)", d)
		}
	}
	return nil
}

// validateChatTarget is the save-time rule that the document names a chat deployment, unless Entra ID with a
// project endpoint lets the listing supply one; out of Validate, which runs on every provider construction.
func (c AzureFoundryConfig) validateChatTarget() error {
	if len(c.chatDeployments()) > 0 || len(c.ClaudeDeployments) > 0 || c.listsProjectDeployments() {
		return nil
	}
	return azureCfgErr(AzureCfgErrMissingDeploy, "name at least one chat deployment (a default chat deployment or additional deployments): an API key cannot list the resource's deployments — only Microsoft Entra ID with a project endpoint can")
}

func validateAzureEndpoint(raw string, hosts azureHostPolicy, project bool) error {
	code := AzureCfgErrInvalidEndpoint
	label := "endpoint"
	if project {
		code = AzureCfgErrInvalidProject
		label = "project endpoint"
	}
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return azureCfgErr(code, "the %s must be an absolute https:// URL", label)
	}
	if u.Scheme != "https" {
		return azureCfgErr(code, "the %s must use https://", label)
	}
	// ForceQuery: a bare trailing "?" that bypassed normalisation would join into "host?/openai/…".
	if u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" {
		return azureCfgErr(code, "the %s must not carry credentials, a query string or a fragment", label)
	}
	if u.Port() != "" && u.Port() != "443" {
		return azureCfgErr(code, "the %s must use the default https port", label)
	}
	if project {
		if !azureProjectPath.MatchString(u.Path) {
			return azureCfgErr(code, "the project endpoint must look like https://<resource>.services.ai.azure.com/api/projects/<project>")
		}
	} else if u.Path != "" {
		return azureCfgErr(code, "the endpoint must be the resource root, without a path (https://<resource>.openai.azure.com)")
	}
	// The serverless refusal runs BEFORE the allow-list: the provider only speaks /openai/v1, which a
	// *.models.ai.azure.com endpoint never serves — the operator gets the migration message, not a dead document.
	if !project && strings.HasSuffix(strings.ToLower(u.Hostname()), ".models.ai.azure.com") {
		return azureCfgErr(code, "serverless API endpoints (*.models.ai.azure.com) are not supported: deploy the model to a Foundry resource and use https://<resource>.services.ai.azure.com")
	}
	if !hosts.allows(u.Hostname()) {
		if project {
			// Its own code: HOST_NOT_ALLOWED is mapped by the client to fixed copy about the RESOURCE endpoint,
			// which would mislead here.
			return azureCfgErr(AzureCfgErrInvalidProject, "the project endpoint host %q is not an Azure AI Foundry host (expected <resource>.services.ai.azure.com; the operator can allow other hosts with PAD_AZURE_FOUNDRY_ALLOWED_HOSTS)", u.Hostname())
		}
		return azureCfgErr(AzureCfgErrHostNotAllowed, "host %q is not an Azure AI Foundry endpoint (expected *.openai.azure.com, *.services.ai.azure.com or *.cognitiveservices.azure.com in the Azure public cloud; the operator can allow other hosts with PAD_AZURE_FOUNDRY_ALLOWED_HOSTS)", u.Hostname())
	}
	return nil
}

// azureHostPolicy is the endpoint allow-list: Microsoft's suffixes plus the operator's extra hosts (exact names
// or "*.suffix" globs) for API Management fronts and private DNS zones.
type azureHostPolicy struct {
	extra []string
}

func (p azureHostPolicy) allows(host string) bool {
	host = strings.ToLower(host)
	// DNS names never contain ':', '%', '[' or ']': refusing them rejects every IP-literal form before the
	// suffix check — including a zoned IPv6 literal whose zone carries an allowed suffix ("[::1%25.openai.
	// azure.com]" dials as [::1]); netip.ParseAddr catches what is left.
	if host == "" || strings.ContainsAny(host, ":%[]") {
		return false
	}
	if _, err := netip.ParseAddr(host); err == nil {
		return false
	}
	for _, s := range azureFoundryHostSuffixes {
		if strings.HasSuffix(host, s) && len(host) > len(s) {
			return true
		}
	}
	for _, e := range p.extra {
		e = strings.ToLower(strings.TrimSpace(e))
		switch {
		case e == "":
			continue
		case strings.HasPrefix(e, "*."):
			if strings.HasSuffix(host, e[1:]) && len(host) > len(e)-1 {
				return true
			}
		case host == e:
			return true
		}
	}
	return false
}

// AzureFoundryOptions is the operator-level configuration applied once at boot (ConfigureAzureFoundry).
type AzureFoundryOptions struct {
	// Egress is the outbound policy: egress.BlockInternal (cloud) installs a dial-time guard refusing private,
	// loopback and link-local addresses for every Azure request — a user-controlled endpoint must not become a
	// probe into the server's network (behind a proxy the guard judges the Azure host, letting the hop through,
	// sound only because every endpoint host is allow-listed). egress.AllowLoopback (desktop) installs none.
	Egress egress.Policy
	// ExtraAllowedHosts extends the endpoint allow-list (PAD_AZURE_FOUNDRY_ALLOWED_HOSTS).
	ExtraAllowedHosts []string
	// AllowPrivateEgress relaxes the dial-time guard to egress.AllowPrivateNetwork — for private-link
	// endpoints. Only RFC 1918 and unique-local addresses open: loopback, link-local (169.254.169.254) and the
	// other special-purpose ranges stay refused.
	AllowPrivateEgress bool
	// Transport is a TEST SEAM (nil in production): the Azure client is built on this RoundTripper instead of
	// the shared transport, bypassing the egress guard whatever Egress says — never set it outside tests.
	Transport http.RoundTripper
}

// azureResponseHeaderTimeout (raised from the shared 30 s) covers a stream-refusing deployment's non-streaming
// fallback, which can reason for minutes before the first header — the shared floor aborted it while the tokens
// were billed, and a header timeout satisfies context.DeadlineExceeded, which the retry wrapper excludes.
var azureResponseHeaderTimeout = 5 * time.Minute

// azureTransport clones base with the Azure ResponseHeaderTimeout applied, so the shared client keeps its own settings.
func azureTransport(base *http.Transport) *http.Transport {
	t := base.Clone()
	t.ResponseHeaderTimeout = azureResponseHeaderTimeout
	return t
}

// sharedTransportBase is the shared client's transport as an *http.Transport, falling back to http.DefaultTransport.
func sharedTransportBase() *http.Transport {
	if t, ok := sharedHTTPClient.Transport.(*http.Transport); ok {
		return t
	}
	return http.DefaultTransport.(*http.Transport)
}

// unconfiguredAzureClient is the zero-value client served until ConfigureAzureFoundry runs: the shared
// transport with redirects refused (the raw sharedHTTPClient FOLLOWS redirects, and a 307/308 could steer a
// credential-bearing request to a host validation never saw) and no dial guard — exactly the desktop/local
// client, so an unconfigured runtime is not a fail-open one.
func unconfiguredAzureClient() *http.Client {
	return &http.Client{Timeout: 0, Transport: azureTransport(sharedTransportBase()), CheckRedirect: egress.NoRedirects}
}

var azureRuntime = struct {
	mu     sync.RWMutex
	opts   AzureFoundryOptions
	client *http.Client
	// configured records that ConfigureAzureFoundry ran. Until then azureHTTPClient serves
	// unconfiguredAzureClient and warns once, so an entrypoint that forgets the operator wiring says so in the log.
	configured bool
	warned     bool
}{client: unconfiguredAzureClient()}

// ConfigureAzureFoundry installs the operator options and builds the HTTP client every Azure AI Foundry
// provider instance uses. Called from DI at boot; tests that construct providers directly never need it.
func ConfigureAzureFoundry(opts AzureFoundryOptions) {
	var client *http.Client
	switch {
	case opts.Transport != nil:
		// Tests only (see AzureFoundryOptions.Transport): no dial guard.
		client = &http.Client{Timeout: 0, Transport: opts.Transport, CheckRedirect: egress.NoRedirects}
	case opts.Egress == egress.BlockInternal:
		base := sharedTransportBase()
		policy := egress.BlockInternal
		if opts.AllowPrivateEgress {
			policy = egress.AllowPrivateNetwork
		}
		t := guardedAzureTransport(base, &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}, policy)
		client = &http.Client{Timeout: 0, Transport: t, CheckRedirect: egress.NoRedirects}
	default:
		client = &http.Client{Timeout: 0, Transport: azureTransport(sharedTransportBase()), CheckRedirect: egress.NoRedirects}
	}
	azureRuntime.mu.Lock()
	azureRuntime.opts = opts
	azureRuntime.client = client
	azureRuntime.configured = true
	azureRuntime.mu.Unlock()
}

// guardedAzureTransport clones base, applies the Azure ResponseHeaderTimeout and installs the egress guard: a
// direct connection is judged on the concrete IP at connect time, a proxied one on the Azure host in the proxy
// hook — trusting the proxy hop is sound only because azureHostPolicy allow-lists every endpoint (tenant-supplied
// targets use egress.GuardTransport, which fails closed instead).
func guardedAzureTransport(base *http.Transport, d *net.Dialer, policy egress.Policy) *http.Transport {
	t := azureTransport(base)
	egress.GuardAllowListedTransport(t, d, policy)
	return t
}

func azureHTTPClient() *http.Client {
	azureRuntime.mu.RLock()
	client, configured := azureRuntime.client, azureRuntime.configured
	azureRuntime.mu.RUnlock()
	if !configured {
		azureRuntime.mu.Lock()
		if !azureRuntime.warned {
			azureRuntime.warned = true
			logger.Warn("azure ai foundry: used before ConfigureAzureFoundry ran — requests use the unguarded shared transport (no dial-time egress guard); DI must install the operator policy at boot")
		}
		azureRuntime.mu.Unlock()
	}
	return client
}

func currentAzureHostPolicy() azureHostPolicy {
	azureRuntime.mu.RLock()
	defer azureRuntime.mu.RUnlock()
	return azureHostPolicy{extra: azureRuntime.opts.ExtraAllowedHosts}
}

// ValidateAzureFoundryConfig validates a document against the operator's host policy plus the save-time
// chat-deployment rule (the API layer calls this before storing).
func ValidateAzureFoundryConfig(cfg AzureFoundryConfig) error {
	if err := cfg.Validate(currentAzureHostPolicy()); err != nil {
		return err
	}
	return cfg.validateChatTarget()
}
