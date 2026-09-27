package ai

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/egress"
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
	// ChatDeployment (optional) is the deployment offered as the default
	// model; EmbeddingDeployment (optional) the one used for the knowledge
	// base.
	ChatDeployment      string `json:"chatDeployment,omitempty"`
	EmbeddingDeployment string `json:"embeddingDeployment,omitempty"`
}

// AzureFoundryPublicConfig is the redacted view returned to clients: every
// field but the secrets, plus whether a secret is stored.
type AzureFoundryPublicConfig struct {
	Configured          bool   `json:"configured"`
	Endpoint            string `json:"endpoint,omitempty"`
	AuthMode            string `json:"authMode,omitempty"`
	HasSecret           bool   `json:"hasSecret"`
	TenantID            string `json:"tenantId,omitempty"`
	ClientID            string `json:"clientId,omitempty"`
	ProjectEndpoint     string `json:"projectEndpoint,omitempty"`
	ChatDeployment      string `json:"chatDeployment,omitempty"`
	EmbeddingDeployment string `json:"embeddingDeployment,omitempty"`
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
	}
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

// Fingerprint identifies the credential set without revealing it — the
// cache key for token sources and model listings (a changed key or endpoint
// must never reuse another's cache).
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
	return u.String()
}

var (
	// azureFoundryHostSuffixes are the DNS suffixes Microsoft issues for
	// Foundry / Azure OpenAI resources. A user-supplied endpoint outside them
	// is refused (the credential would otherwise be posted to an arbitrary
	// host of the user's choosing — with the server's egress).
	azureFoundryHostSuffixes = []string{
		".openai.azure.com",
		".services.ai.azure.com",
		".cognitiveservices.azure.com",
		".ai.azure.com",
	}
	azureDeploymentName = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
	azureProjectPath    = regexp.MustCompile(`^/api/projects/[A-Za-z0-9._-]{1,64}$`)
	azureGUID           = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
)

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
		if !azureGUID.MatchString(c.Auth.TenantID) {
			return azureCfgErr(AzureCfgErrInvalidAuth, "the tenant ID must be a GUID")
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
	for _, d := range []string{c.ChatDeployment, c.EmbeddingDeployment} {
		if d != "" && !azureDeploymentName.MatchString(d) {
			return azureCfgErr(AzureCfgErrInvalidDeploy, "deployment name %q may only contain letters, digits, '.', '_' and '-' (max 64)", d)
		}
	}
	return nil
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
	if u.User != nil || u.RawQuery != "" || u.Fragment != "" {
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
		return azureCfgErr(AzureCfgErrHostNotAllowed, "host %q is not an Azure AI Foundry endpoint (expected *.openai.azure.com or *.services.ai.azure.com; the operator can allow other hosts with PAD_AZURE_FOUNDRY_ALLOWED_HOSTS)", u.Hostname())
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
	if net.ParseIP(host) != nil {
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
	// not become a probe into the server's network. egress.AllowLoopback
	// (desktop/local mode, single user on their own machine) installs none.
	Egress egress.Policy
	// ExtraAllowedHosts extends the endpoint allow-list
	// (PAD_AZURE_FOUNDRY_ALLOWED_HOSTS).
	ExtraAllowedHosts []string
	// AllowPrivateEgress disables the dial-time guard for allow-listed hosts
	// (PAD_AI_ALLOW_PRIVATE_EGRESS) — for private-link endpoints, whose
	// privatelink.* names resolve to RFC 1918 addresses by design.
	AllowPrivateEgress bool
}

var azureRuntime = struct {
	mu     sync.RWMutex
	opts   AzureFoundryOptions
	client *http.Client
}{client: sharedHTTPClient}

// ConfigureAzureFoundry installs the operator options and builds the HTTP
// client every Azure AI Foundry provider instance uses. Called from DI at
// boot; tests that construct providers directly never need it.
func ConfigureAzureFoundry(opts AzureFoundryOptions) {
	var client *http.Client
	if opts.Egress == egress.BlockInternal && !opts.AllowPrivateEgress {
		base, ok := sharedHTTPClient.Transport.(*http.Transport)
		if !ok {
			base = http.DefaultTransport.(*http.Transport)
		}
		t := base.Clone()
		dial := t.DialContext
		if dial == nil {
			dial = (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext
		}
		t.DialContext = egress.GuardDialContext(dial, egress.BlockInternal)
		client = &http.Client{Timeout: 0, Transport: t, CheckRedirect: egress.NoRedirects}
	} else {
		client = &http.Client{Timeout: 0, Transport: sharedHTTPClient.Transport, CheckRedirect: egress.NoRedirects}
	}
	azureRuntime.mu.Lock()
	azureRuntime.opts = opts
	azureRuntime.client = client
	azureRuntime.mu.Unlock()
}

func azureHTTPClient() *http.Client {
	azureRuntime.mu.RLock()
	defer azureRuntime.mu.RUnlock()
	return azureRuntime.client
}

func currentAzureHostPolicy() azureHostPolicy {
	azureRuntime.mu.RLock()
	defer azureRuntime.mu.RUnlock()
	return azureHostPolicy{extra: azureRuntime.opts.ExtraAllowedHosts}
}

// ValidateAzureFoundryConfig validates a document against the operator's
// host policy (the API layer calls this before storing).
func ValidateAzureFoundryConfig(cfg AzureFoundryConfig) error {
	return cfg.Validate(currentAzureHostPolicy())
}
