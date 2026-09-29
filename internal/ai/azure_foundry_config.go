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

// Azure AI Foundry (Microsoft Foundry) is configured per user with a small
// JSON document instead of a bare API key: the resource endpoint, the
// credential (an API key or a Microsoft Entra ID service principal), and
// optional deployment names / project endpoint. The document is what the
// KeyStore holds under the "azure-foundry" provider id, so the factory's
// one-secret-per-provider contract (and every keys.save/has/delete route)
// keeps working unchanged; NewAzureFoundryProvider parses it.

const (
	azureFoundryProviderID = "azure-foundry"
	// AzureFoundryProviderID is the provider id (KeyStore key, request field).
	AzureFoundryProviderID = azureFoundryProviderID

	// AzureAuthModeAPIKey authenticates with the resource's API key (header
	// api-key). Works for model inference only — Foundry Agent Service
	// routes reject keys.
	AzureAuthModeAPIKey = "api_key"
	// AzureAuthModeEntra authenticates as a Microsoft Entra ID service
	// principal (client-credentials flow, scope https://ai.azure.com/.default).
	// Required for agents; works for everything.
	AzureAuthModeEntra = "entra"

	// azureConfigMaxBytes bounds the stored document. The Windows Credential
	// Manager (desktop keyring) rejects secrets above 2560 bytes; a real
	// document is a few hundred.
	azureConfigMaxBytes = 2048

	// azureMaxDeployments bounds the "additional deployments" list so the
	// whole document stays under azureConfigMaxBytes with realistic worst-case
	// values: 64-character resource and project names on the longest host
	// suffixes, an 84-character API key or a 40-character Entra client secret
	// plus both GUIDs, 64-character chat and embedding deployment names, and
	// 16 additional 64-character names encode to about 1750 bytes (20 would
	// leave no room for a longer secret). Encode still enforces the byte cap.
	azureMaxDeployments = 16

	// azureMaxClaudeDeployments bounds the Claude deployments list. Encode
	// still enforces the byte cap: Claude deployments are usually named
	// after their model (about 20 characters), and a document naming the
	// maximum of every list with 64-character names is not a realistic one.
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
	// AzureCfgErrMissingDeploy: the document names no chat deployment
	// (chatDeployment or deployments) and nothing can list one — only Entra
	// ID with a project endpoint can (the project deployments listing).
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
	// TenantID, ClientID and ClientSecret describe the app registration
	// (AzureAuthModeEntra).
	TenantID     string `json:"tenantId,omitempty"`
	ClientID     string `json:"clientId,omitempty"`
	ClientSecret string `json:"clientSecret,omitempty"`
}

// AzureFoundryConfig is the stored per-user document.
type AzureFoundryConfig struct {
	// Endpoint is the resource endpoint, e.g. https://my-res.openai.azure.com
	// or https://my-res.services.ai.azure.com (no path).
	Endpoint string           `json:"endpoint"`
	Auth     AzureFoundryAuth `json:"auth"`
	// ProjectEndpoint (optional) is the Foundry project endpoint,
	// https://<res>.services.ai.azure.com/api/projects/<project>, which
	// unlocks the project's agents (Entra ID only) and deployment metadata.
	ProjectEndpoint string `json:"projectEndpoint,omitempty"`
	// ChatDeployment is the deployment offered as the default model;
	// EmbeddingDeployment (optional) the one used for the knowledge base.
	// Deployments (optional, ≤ azureMaxDeployments) are further chat
	// deployments offered in the model picker. No data-plane route lists an
	// API-key caller's deployments (GET /openai/v1/models is the resource's
	// model CATALOGUE), so the picker shows exactly these names — plus the
	// project's deployments with Entra ID and a project endpoint. Saving
	// requires a chat deployment (ChatDeployment or Deployments) unless
	// Entra ID + project endpoint can list them (MISSING_DEPLOYMENT).
	ChatDeployment      string   `json:"chatDeployment,omitempty"`
	EmbeddingDeployment string   `json:"embeddingDeployment,omitempty"`
	Deployments         []string `json:"deployments,omitempty"`
	// ClaudeDeployments (optional, ≤ azureMaxClaudeDeployments) are the
	// resource's Claude deployments, which answer on the Anthropic Messages
	// route ({endpoint}/anthropic/v1/messages), not on /openai/v1 (404).
	// They are offered in the model picker as "claude:<name>" (see
	// azure_claude.go) and count as chat deployments for the save-time
	// rule. With Entra ID and a project endpoint the project listing also
	// discovers them (modelPublisher "Anthropic").
	ClaudeDeployments []string `json:"claudeDeployments,omitempty"`
}

// AzureFoundryPublicConfig is the redacted view returned to clients: every
// field but the secrets, plus whether a secret is stored.
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

// Configured reports whether the document names an endpoint (an empty
// KeyStore value parses to an unconfigured config).
func (c AzureFoundryConfig) Configured() bool { return c.Endpoint != "" }

// HasSecret reports whether the credential for the selected mode is present.
func (c AzureFoundryConfig) HasSecret() bool {
	switch c.Auth.Mode {
	case AzureAuthModeAPIKey:
		return c.Auth.Key != ""
	case AzureAuthModeEntra:
		return c.Auth.ClientSecret != ""
	}
	return false
}

// Public returns the redacted view.
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

// chatDeployments is every chat deployment the document names: the default
// (ChatDeployment) first, then the additional ones.
func (c AzureFoundryConfig) chatDeployments() []string {
	out := make([]string, 0, 1+len(c.Deployments))
	if c.ChatDeployment != "" {
		out = append(out, c.ChatDeployment)
	}
	return append(out, c.Deployments...)
}

// listsProjectDeployments reports whether the project deployments listing is
// reachable: it needs a project endpoint and Entra ID (the project data plane
// rejects API keys).
func (c AzureFoundryConfig) listsProjectDeployments() bool {
	return c.ProjectEndpoint != "" && c.Auth.Mode == AzureAuthModeEntra
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

// Fingerprint identifies the credential set without revealing it (a changed
// key or endpoint must never reuse another's cache). It deliberately leaves
// the deployment names out; the model-listing cache, whose entries are built
// from them, extends it (azureModelsCacheKey).
func (c AzureFoundryConfig) Fingerprint() string {
	h := sha256.New()
	for _, part := range []string{c.Endpoint, c.ProjectEndpoint, c.Auth.Mode, c.Auth.Key, c.Auth.TenantID, c.Auth.ClientID, c.Auth.ClientSecret} {
		h.Write([]byte(part))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ParseAzureFoundryConfig decodes the stored document. An empty string is a
// valid, unconfigured config (the metadata provider is built with "").
// Unknown fields are rejected so a document written by a newer build is not
// silently truncated by an older one.
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

// Normalize trims every field and canonicalises the endpoints (lower-case
// host, no trailing slash) so equal configurations compare equal. Parse does
// it; the API layer calls it on a freshly decoded request body.
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
	// A name already listed as a chat-completions deployment is dropped
	// here: one deployment answers on one route, and the ordinary lists
	// came first.
	c.ClaudeDeployments = normalizeAzureDeployments(c.ClaudeDeployments, seen)
}

// normalizeAzureDeployments trims a list of deployment names, drops blanks,
// duplicates (case-insensitively — Azure deployment names are) and the names
// in seen (the chat and embedding deployment, and the lists normalised
// before this one), keeping the first occurrence's order and spelling and
// adding what it keeps to seen. nil when nothing is left, so an empty list is
// omitted from the stored document.
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
	// A bare trailing "?" (an address-bar copy) carries nothing; kept,
	// String() re-emits it and every join yields "host?/openai/…" — a 404 on
	// every call after a save that succeeded. A real query string is left for
	// Validate to reject.
	if u.RawQuery == "" {
		u.ForceQuery = false
	}
	// The explicit default port is the default: drop it so equal endpoints
	// compare equal (fingerprint, breaker and memo keys).
	if u.Port() == "443" {
		u.Host = strings.TrimSuffix(u.Host, ":443")
	}
	return u.String()
}

var (
	// azureFoundryHostSuffixes are the DNS suffixes Microsoft documents for
	// Foundry / Azure OpenAI resources in the Azure public cloud (the only
	// cloud supported): the Azure OpenAI and Foundry resource hosts, the
	// Cognitive Services custom subdomain, and serverless (MaaS) model
	// endpoints (<deployment>.<region>.models.ai.azure.com). A user-supplied
	// endpoint outside them is refused (the credential would otherwise be
	// posted to an arbitrary host of the user's choosing — with the server's
	// egress). The broader ".ai.azure.com" is deliberately not listed: it
	// covers Microsoft's own portal and service hosts, which are no
	// resource's endpoint.
	azureFoundryHostSuffixes = []string{
		".openai.azure.com",
		".services.ai.azure.com",
		".cognitiveservices.azure.com",
		".models.ai.azure.com",
	}
	azureDeploymentName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	// azureProjectPath: the project name starts with a letter or digit, so
	// "." and ".." (also as %2e%2e, which url.Parse decodes) can never make
	// the joined project routes climb out of /api/projects/.
	azureProjectPath = regexp.MustCompile(`^/api/projects/[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	azureGUID        = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
	// azureTenantDomain is a tenant's verified domain name
	// (contoso.onmicrosoft.com, contoso.com): lower-case DNS labels, at least
	// two. The input is lower-cased by normalize.
	azureTenantDomain = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
)

// azureMultiTenantAuthorities are the Entra ID pseudo-tenants that are not a
// tenant: a client-credentials sign-in needs the app's home tenant.
var azureMultiTenantAuthorities = map[string]bool{"common": true, "organizations": true, "consumers": true}

// validAzureTenantID accepts a directory (tenant) ID — a GUID — or one of
// the tenant's domain names (contoso.onmicrosoft.com), and refuses the
// multi-tenant pseudo-tenants and IP literals.
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

// Validate checks the document's shape and the endpoint allow-list. It does
// no DNS resolution and no network I/O (the dial-time guard covers runtime),
// so it is cheap enough to run on every provider construction.
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
	default:
		return azureCfgErr(AzureCfgErrInvalidAuth, "unknown authentication mode %q (use %q or %q)", c.Auth.Mode, AzureAuthModeAPIKey, AzureAuthModeEntra)
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

// validateChatTarget is the save-time rule that the document names a chat
// deployment (ChatDeployment, Deployments or ClaudeDeployments), unless
// Entra ID with a project endpoint lets the project deployments listing
// supply them. It is not part of Validate, which also runs on every provider
// construction: a document stored before the rule (or one used only for
// knowledge-base embeddings) keeps working until the user next saves it.
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
	// ForceQuery: a bare trailing "?" that bypassed normalisation would join
	// into "host?/openai/…".
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
	if !hosts.allows(u.Hostname()) {
		if project {
			// Its own code: HOST_NOT_ALLOWED is mapped by the client to fixed
			// copy about the RESOURCE endpoint, which would tell a user whose
			// resource endpoint is fine that it is wrong.
			return azureCfgErr(AzureCfgErrInvalidProject, "the project endpoint host %q is not an Azure AI Foundry host (expected <resource>.services.ai.azure.com; the operator can allow other hosts with PAD_AZURE_FOUNDRY_ALLOWED_HOSTS)", u.Hostname())
		}
		return azureCfgErr(AzureCfgErrHostNotAllowed, "host %q is not an Azure AI Foundry endpoint (expected *.openai.azure.com, *.services.ai.azure.com, *.cognitiveservices.azure.com or *.models.ai.azure.com in the Azure public cloud; the operator can allow other hosts with PAD_AZURE_FOUNDRY_ALLOWED_HOSTS)", u.Hostname())
	}
	return nil
}

// azureHostPolicy is the endpoint allow-list: Microsoft's suffixes plus the
// operator's extra hosts (exact names or "*.suffix" globs) for API Management
// fronts and private DNS zones.
type azureHostPolicy struct {
	extra []string
}

func (p azureHostPolicy) allows(host string) bool {
	host = strings.ToLower(host)
	// DNS names never contain ':', '%', '[' or ']'. Refusing them rejects
	// every IP-literal form before the suffix check, including a zoned IPv6
	// literal whose zone carries an allowed suffix
	// ("https://[::1%25.openai.azure.com]" has the host
	// "::1%.openai.azure.com", which net.ParseIP does not accept but net/http
	// dials as [::1]) and 4-in-6 forms; netip.ParseAddr catches what is left.
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

// AzureFoundryOptions is the operator-level configuration applied once at
// boot (ConfigureAzureFoundry).
type AzureFoundryOptions struct {
	// Egress is the outbound policy: egress.BlockInternal (cloud) installs a
	// dial-time guard that refuses private, loopback and link-local
	// addresses for every Azure request — a user-controlled endpoint must
	// not become a probe into the server's network. Behind an HTTP(S) proxy
	// the guard judges the Azure host in the proxy hook and lets the hop to
	// the proxy through (egress.GuardAllowListedTransport — safe only because
	// every endpoint host is on the allow-list). egress.AllowLoopback
	// (desktop/local mode, single user on their own machine) installs none.
	Egress egress.Policy
	// ExtraAllowedHosts extends the endpoint allow-list
	// (PAD_AZURE_FOUNDRY_ALLOWED_HOSTS).
	ExtraAllowedHosts []string
	// AllowPrivateEgress relaxes the dial-time guard to
	// egress.AllowPrivateNetwork (PAD_AI_ALLOW_PRIVATE_EGRESS) — for
	// private-link endpoints, whose privatelink.* names resolve to RFC 1918
	// addresses by design. Only RFC 1918 and unique-local addresses are
	// opened: loopback, link-local (169.254.169.254), unspecified and the
	// other special-purpose ranges stay refused.
	AllowPrivateEgress bool
	// Transport is a TEST SEAM — nil in production. When non-nil, the Azure
	// client every provider and the Entra ID token requests use
	// (azureHTTPClient) is built on this RoundTripper instead of the shared
	// transport, still refusing redirects (egress.NoRedirects). The egress
	// dial-time guard cannot wrap an arbitrary RoundTripper, so the seam
	// BYPASSES it whatever Egress says: never set it outside tests.
	Transport http.RoundTripper
}

// unconfiguredAzureClient is the zero-value client, served until
// ConfigureAzureFoundry runs (DI at boot; tests and non-DI entrypoints may
// never call it): the shared transport with redirects refused. The zero value
// used to be the raw sharedHTTPClient, which FOLLOWS redirects — before any
// operator policy exists, a 307/308 from a user-supplied endpoint could steer
// a credential-bearing request to a host validation never saw. Refusing
// redirects needs no policy decision, so it applies in that state too; the
// dial-time private-address guard deliberately does not (which policy to
// enforce is the operator's call, and desktop/local mode installs none — a
// LAN-hosted endpoint is a legitimate target there). This is exactly the
// client ConfigureAzureFoundry builds for desktop/local mode, so an
// unconfigured runtime behaves like a local one instead of a fail-open one.
func unconfiguredAzureClient() *http.Client {
	return &http.Client{Timeout: 0, Transport: sharedHTTPClient.Transport, CheckRedirect: egress.NoRedirects}
}

var azureRuntime = struct {
	mu     sync.RWMutex
	opts   AzureFoundryOptions
	client *http.Client
	// configured records that ConfigureAzureFoundry ran. Until then
	// azureHTTPClient serves unconfiguredAzureClient and warns once, so an
	// entrypoint that forgets the operator wiring says so in the log instead
	// of silently dialing user-supplied endpoints unguarded.
	configured bool
	warned     bool
}{client: unconfiguredAzureClient()}

// ConfigureAzureFoundry installs the operator options and builds the HTTP
// client every Azure AI Foundry provider instance uses. Called from DI at
// boot; tests that construct providers directly never need it.
func ConfigureAzureFoundry(opts AzureFoundryOptions) {
	var client *http.Client
	switch {
	case opts.Transport != nil:
		// Tests only (see AzureFoundryOptions.Transport): no dial guard.
		client = &http.Client{Timeout: 0, Transport: opts.Transport, CheckRedirect: egress.NoRedirects}
	case opts.Egress == egress.BlockInternal:
		base, ok := sharedHTTPClient.Transport.(*http.Transport)
		if !ok {
			base = http.DefaultTransport.(*http.Transport)
		}
		policy := egress.BlockInternal
		if opts.AllowPrivateEgress {
			policy = egress.AllowPrivateNetwork
		}
		t := guardedAzureTransport(base, &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}, policy)
		client = &http.Client{Timeout: 0, Transport: t, CheckRedirect: egress.NoRedirects}
	default:
		client = &http.Client{Timeout: 0, Transport: sharedHTTPClient.Transport, CheckRedirect: egress.NoRedirects}
	}
	azureRuntime.mu.Lock()
	azureRuntime.opts = opts
	azureRuntime.client = client
	azureRuntime.configured = true
	azureRuntime.mu.Unlock()
}

// guardedAzureTransport clones base (keeping its pool settings and its Proxy
// function — HTTP(S)_PROXY for the shared client) and installs the egress
// guard under policy (egress.BlockInternal, or egress.AllowPrivateNetwork
// for PAD_AI_ALLOW_PRIVATE_EGRESS) with d as the dialer: a direct connection is judged on the concrete
// IP at connect time; a proxied request is judged on the Azure host in the
// proxy hook, and the hop to the operator's proxy goes through (a proxy on a
// private address is the normal corporate setup, and judging it refused every
// Azure call while leaving the real target unchecked). Trusting the proxy hop
// is sound here only because azureHostPolicy restricts every endpoint to
// allow-listed hosts whose DNS no tenant controls; tenant-supplied targets
// (notify webhooks) use egress.GuardTransport, which fails closed instead.
func guardedAzureTransport(base *http.Transport, d *net.Dialer, policy egress.Policy) *http.Transport {
	t := base.Clone()
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

// ValidateAzureFoundryConfig validates a document against the operator's
// host policy plus the save-time chat-deployment rule (the API layer calls
// this before storing).
func ValidateAzureFoundryConfig(cfg AzureFoundryConfig) error {
	if err := cfg.Validate(currentAzureHostPolicy()); err != nil {
		return err
	}
	return cfg.validateChatTarget()
}
