package ai

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/egress"
)

func azureCfgCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var ce *AzureConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("error is not an *AzureConfigError: %v", err)
	}
	return ce.Code
}

func TestParseAzureFoundryConfig(t *testing.T) {
	t.Run("empty is unconfigured", func(t *testing.T) {
		cfg, err := ParseAzureFoundryConfig("")
		if err != nil || cfg.Configured() {
			t.Fatalf("empty: cfg=%+v err=%v", cfg, err)
		}
	})
	t.Run("normalises the endpoint", func(t *testing.T) {
		cfg, err := ParseAzureFoundryConfig(`{"endpoint":" HTTPS://My-Res.OpenAI.Azure.com/ ","auth":{"mode":"API_KEY","key":" k1 "},"chatDeployment":" gpt-4.1 "}`)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Endpoint != "https://my-res.openai.azure.com" {
			t.Errorf("endpoint = %q", cfg.Endpoint)
		}
		if cfg.Auth.Mode != AzureAuthModeAPIKey || cfg.Auth.Key != "k1" || cfg.ChatDeployment != "gpt-4.1" {
			t.Errorf("auth/deployment not trimmed: %+v", cfg)
		}
	})
	// D-50: an address-bar copy ending in "?" (url.ForceQuery) used to survive
	// normalisation and validation and join into "host?/openai/…" — a 404 on
	// every call after a successful save; the explicit default port is
	// dropped so equal endpoints compare equal.
	t.Run("drops a bare trailing question mark and the default port", func(t *testing.T) {
		cfg, err := ParseAzureFoundryConfig(`{"endpoint":"https://my-res.openai.azure.com/?","auth":{"mode":"entra","tenantId":"11111111-2222-3333-4444-555555555555","clientId":"aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee","clientSecret":"s"},"projectEndpoint":"https://my-res.services.ai.azure.com:443/api/projects/p?"}`)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.Endpoint != "https://my-res.openai.azure.com" {
			t.Errorf("endpoint = %q", cfg.Endpoint)
		}
		if cfg.ProjectEndpoint != "https://my-res.services.ai.azure.com/api/projects/p" {
			t.Errorf("project endpoint = %q", cfg.ProjectEndpoint)
		}
		if err := cfg.Validate(azureHostPolicy{}); err != nil {
			t.Errorf("normalised document must validate: %v", err)
		}
		if got := normalizeAzureURL("https://my-res.openai.azure.com:443"); got != "https://my-res.openai.azure.com" {
			t.Errorf("explicit :443 = %q", got)
		}
		if got := normalizeAzureURL("https://my-res.openai.azure.com/?x=1"); got != "https://my-res.openai.azure.com?x=1" {
			t.Errorf("a real query string must be kept for Validate: %q", got)
		}
	})
	t.Run("rejects unknown fields", func(t *testing.T) {
		_, err := ParseAzureFoundryConfig(`{"endpoint":"https://a.openai.azure.com","auth":{"mode":"api_key","key":"k"},"bogus":1}`)
		if azureCfgCode(t, err) != AzureCfgErrInvalidConfig {
			t.Errorf("want INVALID_CONFIG, got %v", err)
		}
	})
	t.Run("rejects oversized documents", func(t *testing.T) {
		_, err := ParseAzureFoundryConfig(`{"endpoint":"https://a.openai.azure.com","auth":{"mode":"api_key","key":"` + strings.Repeat("k", 3000) + `"}}`)
		if azureCfgCode(t, err) != AzureCfgErrTooLarge {
			t.Errorf("want CONFIG_TOO_LARGE, got %v", err)
		}
	})
}

func TestAzureFoundryConfig_Validate(t *testing.T) {
	policy := azureHostPolicy{extra: []string{"gateway.example.com", "*.corp.example"}}
	apiKey := AzureFoundryAuth{Mode: AzureAuthModeAPIKey, Key: "k1"}
	entra := AzureFoundryAuth{Mode: AzureAuthModeEntra, TenantID: "11111111-2222-3333-4444-555555555555", ClientID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", ClientSecret: "s"}

	cases := []struct {
		name string
		cfg  AzureFoundryConfig
		want string // error code, "" for valid
	}{
		{"api key on openai host", AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", Auth: apiKey}, ""},
		{"entra on services host with project", AzureFoundryConfig{Endpoint: "https://my-res.services.ai.azure.com", Auth: entra, ProjectEndpoint: "https://my-res.services.ai.azure.com/api/projects/proj_1"}, ""},
		{"operator exact host", AzureFoundryConfig{Endpoint: "https://gateway.example.com", Auth: apiKey}, ""},
		{"operator glob host", AzureFoundryConfig{Endpoint: "https://ai.corp.example", Auth: apiKey}, ""},
		{"missing endpoint", AzureFoundryConfig{Auth: apiKey}, AzureCfgErrInvalidEndpoint},
		{"http scheme", AzureFoundryConfig{Endpoint: "http://my-res.openai.azure.com", Auth: apiKey}, AzureCfgErrInvalidEndpoint},
		{"userinfo", AzureFoundryConfig{Endpoint: "https://u:p@my-res.openai.azure.com", Auth: apiKey}, AzureCfgErrInvalidEndpoint},
		{"path on resource endpoint", AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com/openai/v1", Auth: apiKey}, AzureCfgErrInvalidEndpoint},
		{"query string", AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com?x=1", Auth: apiKey}, AzureCfgErrInvalidEndpoint},
		{"query string after a slash", AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com/?x=1", Auth: apiKey}, AzureCfgErrInvalidEndpoint},
		{"explicit default port", AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com:443", Auth: apiKey}, ""},
		{"bare trailing question mark", AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com/?", Auth: apiKey}, ""},
		{"foreign host", AzureFoundryConfig{Endpoint: "https://evil.example", Auth: apiKey}, AzureCfgErrHostNotAllowed},
		{"suffix without subdomain", AzureFoundryConfig{Endpoint: "https://openai.azure.com", Auth: apiKey}, AzureCfgErrHostNotAllowed},
		{"metadata ip", AzureFoundryConfig{Endpoint: "https://169.254.169.254", Auth: apiKey}, AzureCfgErrHostNotAllowed},
		{"loopback name", AzureFoundryConfig{Endpoint: "https://localhost", Auth: apiKey}, AzureCfgErrHostNotAllowed},
		{"project path wrong", AzureFoundryConfig{Endpoint: "https://my-res.services.ai.azure.com", Auth: entra, ProjectEndpoint: "https://my-res.services.ai.azure.com/projects/p"}, AzureCfgErrInvalidProject},
		// D-51: a project-host rejection carries the PROJECT code — the
		// client maps HOST_NOT_ALLOWED to fixed copy about the resource
		// endpoint, which would blame an endpoint that is fine.
		{"project foreign host", AzureFoundryConfig{Endpoint: "https://my-res.services.ai.azure.com", Auth: entra, ProjectEndpoint: "https://evil.example/api/projects/p"}, AzureCfgErrInvalidProject},
		{"api key missing", AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", Auth: AzureFoundryAuth{Mode: AzureAuthModeAPIKey}}, AzureCfgErrMissingSecret},
		{"entra secret missing", AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra, TenantID: entra.TenantID, ClientID: entra.ClientID}}, AzureCfgErrMissingSecret},
		{"entra tenant not a guid", AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra, TenantID: "contoso", ClientID: entra.ClientID, ClientSecret: "s"}}, AzureCfgErrInvalidAuth},
		{"entra tenant domain", AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra, TenantID: "Contoso.onmicrosoft.com", ClientID: entra.ClientID, ClientSecret: "s"}}, ""},
		{"entra tenant custom domain", AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra, TenantID: "ai-team.contoso-corp.com", ClientID: entra.ClientID, ClientSecret: "s"}}, ""},
		{"entra tenant common", AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra, TenantID: "common", ClientID: entra.ClientID, ClientSecret: "s"}}, AzureCfgErrInvalidAuth},
		{"entra tenant organizations", AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra, TenantID: "organizations", ClientID: entra.ClientID, ClientSecret: "s"}}, AzureCfgErrInvalidAuth},
		{"entra tenant consumers", AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra, TenantID: "consumers", ClientID: entra.ClientID, ClientSecret: "s"}}, AzureCfgErrInvalidAuth},
		{"entra tenant path", AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra, TenantID: "contoso.com/../x", ClientID: entra.ClientID, ClientSecret: "s"}}, AzureCfgErrInvalidAuth},
		{"entra tenant ip", AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra, TenantID: "10.0.0.1", ClientID: entra.ClientID, ClientSecret: "s"}}, AzureCfgErrInvalidAuth},
		{"entra tenant dotted edge", AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra, TenantID: ".contoso.com", ClientID: entra.ClientID, ClientSecret: "s"}}, AzureCfgErrInvalidAuth},
		{"entra tenant hyphen label", AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra, TenantID: "-contoso.com", ClientID: entra.ClientID, ClientSecret: "s"}}, AzureCfgErrInvalidAuth},
		// The project name starts with a letter or digit: no "." / ".."
		// segment, whether typed or percent-encoded.
		{"project dot dot", AzureFoundryConfig{Endpoint: "https://my-res.services.ai.azure.com", Auth: entra, ProjectEndpoint: "https://my-res.services.ai.azure.com/api/projects/.."}, AzureCfgErrInvalidProject},
		{"project encoded dot dot", AzureFoundryConfig{Endpoint: "https://my-res.services.ai.azure.com", Auth: entra, ProjectEndpoint: "https://my-res.services.ai.azure.com/api/projects/%2e%2e"}, AzureCfgErrInvalidProject},
		{"project dot", AzureFoundryConfig{Endpoint: "https://my-res.services.ai.azure.com", Auth: entra, ProjectEndpoint: "https://my-res.services.ai.azure.com/api/projects/."}, AzureCfgErrInvalidProject},
		{"project leading dash", AzureFoundryConfig{Endpoint: "https://my-res.services.ai.azure.com", Auth: entra, ProjectEndpoint: "https://my-res.services.ai.azure.com/api/projects/-p"}, AzureCfgErrInvalidProject},
		{"project digit first", AzureFoundryConfig{Endpoint: "https://my-res.services.ai.azure.com", Auth: entra, ProjectEndpoint: "https://my-res.services.ai.azure.com/api/projects/1proj.v2"}, ""},
		// Serverless (MaaS) endpoints are refused outright (F-09): the
		// provider speaks only /openai/v1, which they do not serve;
		// Microsoft's other *.ai.azure.com hosts are no resource's endpoint.
		{"serverless models host", AzureFoundryConfig{Endpoint: "https://phi-4-xyz.eastus2.models.ai.azure.com", Auth: apiKey}, AzureCfgErrInvalidEndpoint},
		{"portal host", AzureFoundryConfig{Endpoint: "https://ai.azure.com", Auth: apiKey}, AzureCfgErrHostNotAllowed},
		{"other ai.azure.com host", AzureFoundryConfig{Endpoint: "https://foo.ai.azure.com", Auth: apiKey}, AzureCfgErrHostNotAllowed},
		{"unknown mode", AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", Auth: AzureFoundryAuth{Mode: "managed"}}, AzureCfgErrInvalidAuth},
		{"bad deployment name", AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", Auth: apiKey, ChatDeployment: "my deployment!"}, AzureCfgErrInvalidDeploy},
		// Finding #14: IP literals never pass, whatever suffix a zone carries.
		{"zoned ipv6 loopback", AzureFoundryConfig{Endpoint: "https://[::1%25.openai.azure.com]", Auth: apiKey}, AzureCfgErrHostNotAllowed},
		{"zoned 4-in-6 metadata", AzureFoundryConfig{Endpoint: "https://[::ffff:169.254.169.254%25.openai.azure.com]", Auth: apiKey}, AzureCfgErrHostNotAllowed},
		{"plain ipv6 literal", AzureFoundryConfig{Endpoint: "https://[::1]", Auth: apiKey}, AzureCfgErrHostNotAllowed},
		{"zoned ipv6 project", AzureFoundryConfig{Endpoint: "https://my-res.services.ai.azure.com", Auth: entra, ProjectEndpoint: "https://[fd00::1%25x.services.ai.azure.com]/api/projects/p"}, AzureCfgErrInvalidProject},
		{"zoned 4-in-6 project", AzureFoundryConfig{Endpoint: "https://my-res.services.ai.azure.com", Auth: entra, ProjectEndpoint: "https://[::ffff:a9fe:a9fe%25x.services.ai.azure.com]/api/projects/p"}, AzureCfgErrInvalidProject},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.normalize()
			got := azureCfgCode(t, cfg.Validate(policy))
			if got != tc.want {
				t.Errorf("Validate() code = %q, want %q", got, tc.want)
			}
		})
	}
	// The project-host message names the project endpoint, not the resource.
	bad := AzureFoundryConfig{Endpoint: "https://my-res.services.ai.azure.com", Auth: entra, ProjectEndpoint: "https://apim.contoso.com/api/projects/p"}
	if err := bad.Validate(policy); err == nil || !strings.Contains(err.Error(), "project endpoint host") {
		t.Errorf("project host rejection = %v, want the project-endpoint wording", err)
	}
	// A document that bypassed normalisation (ForceQuery kept) is refused by
	// Validate itself.
	unnormalised := AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com?", Auth: apiKey}
	if got := azureCfgCode(t, unnormalised.Validate(policy)); got != AzureCfgErrInvalidEndpoint {
		t.Errorf("bare '?' without normalisation: code %q, want INVALID_ENDPOINT", got)
	}
	unnormalisedProject := AzureFoundryConfig{Endpoint: "https://my-res.services.ai.azure.com", Auth: entra, ProjectEndpoint: "https://my-res.services.ai.azure.com/api/projects/p?"}
	if got := azureCfgCode(t, unnormalisedProject.Validate(policy)); got != AzureCfgErrInvalidProject {
		t.Errorf("bare '?' on the project endpoint without normalisation: code %q, want INVALID_PROJECT_ENDPOINT", got)
	}
}

func TestAzureFoundryConfig_PublicRedactsSecrets(t *testing.T) {
	cfg := AzureFoundryConfig{
		Endpoint:            "https://my-res.openai.azure.com",
		Auth:                AzureFoundryAuth{Mode: AzureAuthModeEntra, TenantID: "t", ClientID: "c", ClientSecret: "very-secret"},
		ProjectEndpoint:     "https://my-res.services.ai.azure.com/api/projects/p",
		ChatDeployment:      "chat",
		EmbeddingDeployment: "embed",
	}
	pub := cfg.Public()
	if !pub.Configured || !pub.HasSecret || pub.AuthMode != AzureAuthModeEntra || pub.TenantID != "t" || pub.ClientID != "c" ||
		pub.ProjectEndpoint != cfg.ProjectEndpoint || pub.ChatDeployment != "chat" || pub.EmbeddingDeployment != "embed" {
		t.Errorf("public view lost fields: %+v", pub)
	}
	raw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(raw, "very-secret") {
		t.Error("Encode must keep the secret (it is the stored document)")
	}
	if p := (AzureFoundryConfig{}).Public(); p.Configured || p.HasSecret {
		t.Errorf("unconfigured public view = %+v", p)
	}
}

func TestAzureFoundryConfig_FingerprintTracksCredential(t *testing.T) {
	a := AzureFoundryConfig{Endpoint: "https://a.openai.azure.com", Auth: AzureFoundryAuth{Mode: AzureAuthModeAPIKey, Key: "k1"}}
	b := a
	b.Auth.Key = "k2"
	c := a
	c.ChatDeployment = "other" // not part of the credential
	c.Deployments = []string{"extra"}
	if a.Fingerprint() == b.Fingerprint() {
		t.Error("a rotated key must change the fingerprint")
	}
	if a.Fingerprint() != c.Fingerprint() {
		t.Error("a deployment name must not change the fingerprint")
	}
	if azureModelsCacheKey(a) == azureModelsCacheKey(c) {
		t.Error("the listing cache key must change with the deployment names")
	}
}

// The "additional deployments" list: normalised (trimmed, blanks and
// duplicates dropped case-insensitively, the chat and embedding deployment
// removed), validated like the other names, bounded, and round-tripped.
func TestAzureFoundryConfig_Deployments(t *testing.T) {
	cfg, err := ParseAzureFoundryConfig(`{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key","key":"k"},` +
		`"chatDeployment":"Chat1","embeddingDeployment":"emb","deployments":[" gpt-4o ","","GPT-4O","chat1","EMB","phi-4"," "]}`)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(cfg.Deployments, ",") != "gpt-4o,phi-4" {
		t.Errorf("normalised deployments = %q", cfg.Deployments)
	}
	if got := cfg.chatDeployments(); strings.Join(got, ",") != "Chat1,gpt-4o,phi-4" {
		t.Errorf("chat deployments = %v", got)
	}
	if pub := cfg.Public(); strings.Join(pub.Deployments, ",") != "gpt-4o,phi-4" {
		t.Errorf("public view deployments = %v", pub.Deployments)
	}
	if raw, err := cfg.Encode(); err != nil || !strings.Contains(raw, `"deployments":["gpt-4o","phi-4"]`) {
		t.Errorf("encoded = %s, %v", raw, err)
	}
	empty := AzureFoundryConfig{Deployments: []string{" ", ""}}
	empty.normalize()
	if empty.Deployments != nil {
		t.Errorf("all-blank list must normalise to nil (omitted), got %#v", empty.Deployments)
	}
	if raw, _ := (AzureFoundryConfig{Endpoint: "https://a.openai.azure.com"}).Encode(); strings.Contains(raw, "deployments") {
		t.Errorf("an empty list must be omitted: %s", raw)
	}

	policy := azureHostPolicy{}
	base := AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", Auth: AzureFoundryAuth{Mode: AzureAuthModeAPIKey, Key: "k"}}
	bad := base
	bad.Deployments = []string{"ok", "not ok!"}
	if code := azureCfgCode(t, bad.Validate(policy)); code != AzureCfgErrInvalidDeploy {
		t.Errorf("bad additional name: code %q", code)
	}
	many := base
	for i := 0; i <= azureMaxDeployments; i++ {
		many.Deployments = append(many.Deployments, fmt.Sprintf("dep-%02d", i))
	}
	if code := azureCfgCode(t, many.Validate(policy)); code != AzureCfgErrInvalidDeploy {
		t.Errorf("%d additional deployments: code %q, want INVALID_DEPLOYMENT", len(many.Deployments), code)
	}
}

// The documented bound: the longest realistic document with the maximum
// number of additional deployments still fits the stored-document cap.
func TestAzureFoundryConfig_MaxDeploymentsFitTheByteCap(t *testing.T) {
	name := func(prefix string) string { return prefix + strings.Repeat("x", 64-len(prefix)) }
	res := strings.Repeat("r", 64)
	for _, auth := range []AzureFoundryAuth{
		{Mode: AzureAuthModeAPIKey, Key: strings.Repeat("k", 84)},
		{Mode: AzureAuthModeEntra, TenantID: "11111111-2222-3333-4444-555555555555", ClientID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", ClientSecret: strings.Repeat("s", 40)},
	} {
		cfg := AzureFoundryConfig{
			Endpoint:            "https://" + res + ".cognitiveservices.azure.com",
			ProjectEndpoint:     "https://" + res + ".services.ai.azure.com/api/projects/" + strings.Repeat("p", 64),
			Auth:                auth,
			ChatDeployment:      name("chat"),
			EmbeddingDeployment: name("emb"),
		}
		for i := 0; i < azureMaxDeployments; i++ {
			cfg.Deployments = append(cfg.Deployments, name(fmt.Sprintf("d%02d", i)))
		}
		cfg.normalize()
		if len(cfg.Deployments) != azureMaxDeployments {
			t.Fatalf("fixture lost deployments: %d", len(cfg.Deployments))
		}
		if err := cfg.Validate(azureHostPolicy{}); err != nil {
			t.Errorf("%s: Validate: %v", auth.Mode, err)
		}
		if raw, err := cfg.Encode(); err != nil {
			t.Errorf("%s: worst-case document (%d bytes) must fit the %d-byte cap: %v", auth.Mode, len(raw), azureConfigMaxBytes, err)
		}
	}
}

// MISSING_DEPLOYMENT is a save-time rule: an API key cannot list deployments,
// so a chat deployment must be named unless Entra ID + project can list them.
// Provider construction (Validate) keeps accepting older documents.
func TestValidateAzureFoundryConfig_RequiresAChatDeployment(t *testing.T) {
	apiKey := AzureFoundryAuth{Mode: AzureAuthModeAPIKey, Key: "k"}
	entra := AzureFoundryAuth{Mode: AzureAuthModeEntra, TenantID: "11111111-2222-3333-4444-555555555555", ClientID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee", ClientSecret: "s"}
	const endpoint = "https://my-res.services.ai.azure.com"
	const project = endpoint + "/api/projects/proj"
	for _, tc := range []struct {
		name string
		cfg  AzureFoundryConfig
		want string
	}{
		{"api key, nothing named", AzureFoundryConfig{Endpoint: endpoint, Auth: apiKey}, AzureCfgErrMissingDeploy},
		{"api key, embedding only", AzureFoundryConfig{Endpoint: endpoint, Auth: apiKey, EmbeddingDeployment: "emb"}, AzureCfgErrMissingDeploy},
		{"api key with project, nothing named", AzureFoundryConfig{Endpoint: endpoint, Auth: apiKey, ProjectEndpoint: project}, AzureCfgErrMissingDeploy},
		{"entra without project, nothing named", AzureFoundryConfig{Endpoint: endpoint, Auth: entra}, AzureCfgErrMissingDeploy},
		{"api key, chat deployment", AzureFoundryConfig{Endpoint: endpoint, Auth: apiKey, ChatDeployment: "chat"}, ""},
		{"api key, additional deployments only", AzureFoundryConfig{Endpoint: endpoint, Auth: apiKey, Deployments: []string{"a"}}, ""},
		{"entra with project, nothing named", AzureFoundryConfig{Endpoint: endpoint, Auth: entra, ProjectEndpoint: project}, ""},
		{"earlier error wins", AzureFoundryConfig{Endpoint: "http://my-res.openai.azure.com", Auth: apiKey}, AzureCfgErrInvalidEndpoint},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tc.cfg
			cfg.normalize()
			if got := azureCfgCode(t, ValidateAzureFoundryConfig(cfg)); got != tc.want {
				t.Errorf("code = %q, want %q", got, tc.want)
			}
		})
	}
	legacy := NewAzureFoundryProvider(`{"endpoint":"https://my-res.openai.azure.com","auth":{"mode":"api_key","key":"k"},"embeddingDeployment":"emb"}`)
	if legacy.configErr != nil {
		t.Errorf("a stored document without a chat deployment must still construct: %v", legacy.configErr)
	}
}

func TestAzureHostPolicy(t *testing.T) {
	p := azureHostPolicy{extra: []string{"Gateway.Example.com", "*.corp.example", ""}}
	for host, want := range map[string]bool{
		"my-res.openai.azure.com":            true,
		"my-res.services.ai.azure.com":       true,
		"my-res.cognitiveservices.azure.com": true,
		"dep.eastus2.models.ai.azure.com":    false,
		"models.ai.azure.com":                false,
		"foo.ai.azure.com":                   false,
		"ai.azure.com":                       false,
		"gateway.example.com":                true,
		"api.corp.example":                   true,
		"corp.example":                       false,
		"openai.azure.com":                   false,
		"evil.example":                       false,
		"127.0.0.1":                          false,
		"my-res.openai.azure.com.evil.com":   false,
		// Finding #14: zoned IPv6 and 4-in-6 literals carrying an allowed
		// suffix in the zone, and bare IPv6 literals.
		"::1%.openai.azure.com":             false,
		"::ffff:10.0.0.5%.openai.azure.com": false,
		"fd00::1%x.services.ai.azure.com":   false,
		"::ffff:a9fe:a9fe%.ai.azure.com":    false,
		"::1":                               false,
		"[::1]":                             false,
		"":                                  false,
	} {
		if got := p.allows(host); got != want {
			t.Errorf("allows(%q) = %v, want %v", host, got, want)
		}
	}
}

// Finding #13: behind an HTTP(S) proxy on a private address (a corporate
// egress proxy; here on loopback) the guarded Azure client reaches the
// resource through the proxy instead of refusing the proxy's own address, and
// a target that resolves to an internal address is refused in the proxy hook
// without ever reaching the proxy.
func TestGuardedAzureTransport_BehindAPrivateProxy(t *testing.T) {
	var proxied atomic.Int32
	var lastTarget atomic.Value
	proxy := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		proxied.Add(1)
		lastTarget.Store(r.URL.String())
		writeJSON(w, http.StatusOK, chatCompletionJSON("via proxy", "gpt-4o"))
	}))
	t.Cleanup(proxy.Close)
	proxyURL, err := url.Parse(proxy.URL)
	if err != nil {
		t.Fatal(err)
	}
	// What HTTPS_PROXY=http://<private address> selects. The shared client's
	// http.ProxyFromEnvironment reads the environment once per process and
	// never proxies loopback targets, so the test sets the proxy function.
	base := sharedHTTPClient.Transport.(*http.Transport).Clone()
	base.Proxy = http.ProxyURL(proxyURL)
	client := &http.Client{
		Transport:     guardedAzureTransport(base, &net.Dialer{Timeout: 2 * time.Second}, egress.BlockInternal),
		CheckRedirect: egress.NoRedirects,
	}
	chat := func(endpoint string) (*Response, error) {
		ResetAzureFoundryCaches()
		cfg := azureKeyConfig(endpoint)
		cfg.ChatDeployment = "chat1"
		p := newAzureFoundryProvider(cfg, nil, client)
		return p.Chat(context.Background(), Request{Model: "chat1", Messages: []Message{{Role: "user", Content: "hi"}}})
	}

	// A public resource address (documentation range, a literal: no DNS);
	// plain http so the proxy can answer for it without a CONNECT tunnel.
	resp, err := chat("http://203.0.113.10")
	if err != nil {
		t.Fatalf("an Azure call through a private proxy must not be refused: %v", err)
	}
	if resp.Content != "via proxy" || proxied.Load() != 1 || lastTarget.Load() != "http://203.0.113.10/openai/v1/chat/completions" {
		t.Errorf("content=%q proxied=%d target=%v", resp.Content, proxied.Load(), lastTarget.Load())
	}

	// Targets that are, or resolve to, internal addresses are refused before
	// the proxy ("localhost" resolves locally, without the network).
	for _, endpoint := range []string{"http://localhost", "http://10.0.0.7", "http://169.254.169.254"} {
		if _, err := chat(endpoint); !errors.Is(err, egress.ErrInternalAddress) {
			t.Errorf("%s: want an internal-address refusal, got %v", endpoint, err)
		}
	}
	if proxied.Load() != 1 {
		t.Errorf("a refused target reached the proxy (%d requests)", proxied.Load())
	}
}

// ConfigureAzureFoundry keeps the shared transport's proxy selection
// (HTTP(S)_PROXY) on the guarded cloud client and installs the guard; every
// mode's transport is a distinct clone carrying the Azure
// ResponseHeaderTimeout while the shared client keeps its 30 s floor.
func TestConfigureAzureFoundry_GuardedClientKeepsTheProxy(t *testing.T) {
	azureRuntime.mu.RLock()
	prevOpts, prevClient := azureRuntime.opts, azureRuntime.client
	azureRuntime.mu.RUnlock()
	t.Cleanup(func() {
		azureRuntime.mu.Lock()
		azureRuntime.opts, azureRuntime.client = prevOpts, prevClient
		azureRuntime.mu.Unlock()
	})

	ConfigureAzureFoundry(AzureFoundryOptions{Egress: egress.BlockInternal})
	tr, ok := azureHTTPClient().Transport.(*http.Transport)
	if !ok || tr == sharedHTTPClient.Transport {
		t.Fatalf("cloud mode needs its own guarded transport, got %T", azureHTTPClient().Transport)
	}
	if tr.Proxy == nil || tr.DialContext == nil {
		t.Error("the guarded transport must keep a proxy function and install the dial guard")
	}
	if tr.ResponseHeaderTimeout != azureResponseHeaderTimeout {
		t.Errorf("guarded transport ResponseHeaderTimeout = %v, want %v", tr.ResponseHeaderTimeout, azureResponseHeaderTimeout)
	}
	if _, err := tr.DialContext(context.Background(), "tcp", "169.254.169.254:443"); !errors.Is(err, egress.ErrInternalAddress) {
		t.Errorf("direct dials must stay guarded, got %v", err)
	}

	// PAD_AI_ALLOW_PRIVATE_EGRESS keeps a guarded transport: RFC 1918 and
	// unique-local addresses (private-link endpoints) are dialled, loopback,
	// link-local metadata and unspecified addresses still are not.
	ConfigureAzureFoundry(AzureFoundryOptions{Egress: egress.BlockInternal, AllowPrivateEgress: true})
	tr, ok = azureHTTPClient().Transport.(*http.Transport)
	if !ok || tr == sharedHTTPClient.Transport || tr.DialContext == nil {
		t.Fatalf("PAD_AI_ALLOW_PRIVATE_EGRESS must keep a guarded transport, got %T", azureHTTPClient().Transport)
	}
	if tr.ResponseHeaderTimeout != azureResponseHeaderTimeout {
		t.Errorf("PAD_AI_ALLOW_PRIVATE_EGRESS transport ResponseHeaderTimeout = %v, want %v", tr.ResponseHeaderTimeout, azureResponseHeaderTimeout)
	}
	for _, addr := range []string{"169.254.169.254:443", "127.0.0.1:443", "[::1]:443", "0.0.0.0:443", "168.63.129.16:443", "[fe80::1]:443"} {
		if _, err := tr.DialContext(context.Background(), "tcp", addr); !errors.Is(err, egress.ErrInternalAddress) {
			t.Errorf("%s under PAD_AI_ALLOW_PRIVATE_EGRESS: want an internal-address refusal, got %v", addr, err)
		}
	}
	// A private address passes the guard (the dial itself then fails fast:
	// nothing listens on the port in a test run, or the network is absent).
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	if _, err := tr.DialContext(ctx, "tcp", "10.255.255.1:9"); errors.Is(err, egress.ErrInternalAddress) {
		t.Errorf("an RFC 1918 address must pass under PAD_AI_ALLOW_PRIVATE_EGRESS, got %v", err)
	}

	ConfigureAzureFoundry(AzureFoundryOptions{Egress: egress.AllowLoopback})
	local, ok := azureHTTPClient().Transport.(*http.Transport)
	if !ok || local == sharedHTTPClient.Transport {
		t.Fatalf("desktop/local mode needs its own Azure transport, got %T", azureHTTPClient().Transport)
	}
	if local.Proxy == nil {
		t.Error("the local transport must keep the proxy function")
	}
	// No dial guard in local mode: the transport keeps the shared client's
	// DialContext (nil = the default dialer), which never answers with the
	// egress refusal.
	if local.DialContext != nil {
		t.Error("desktop/local mode installs no dial guard")
	}
	if local.ResponseHeaderTimeout != azureResponseHeaderTimeout {
		t.Errorf("local transport ResponseHeaderTimeout = %v, want %v", local.ResponseHeaderTimeout, azureResponseHeaderTimeout)
	}
	if shared, ok := sharedHTTPClient.Transport.(*http.Transport); !ok || shared.ResponseHeaderTimeout != 30*time.Second {
		t.Error("the shared client must keep its 30 s ResponseHeaderTimeout")
	}
}

// saveAzureRuntime configures the runtime for one test and restores the full
// previous state afterwards, so a test's ConfigureAzureFoundry call cannot
// leak its client into the rest of the package's tests.
func saveAzureRuntime(t *testing.T, opts AzureFoundryOptions) {
	t.Helper()
	azureRuntime.mu.RLock()
	prevOpts, prevClient := azureRuntime.opts, azureRuntime.client
	prevConfigured, prevWarned := azureRuntime.configured, azureRuntime.warned
	azureRuntime.mu.RUnlock()
	ConfigureAzureFoundry(opts)
	t.Cleanup(func() {
		azureRuntime.mu.Lock()
		azureRuntime.opts, azureRuntime.client = prevOpts, prevClient
		azureRuntime.configured, azureRuntime.warned = prevConfigured, prevWarned
		azureRuntime.mu.Unlock()
	})
}

// Before ConfigureAzureFoundry runs (non-DI entrypoints) the runtime used to
// hand out the raw sharedHTTPClient — no guard AND redirects followed. The
// zero-value posture is now the local-mode one (an unguarded clone of the
// shared transport with the Azure header timeout, redirects refused), and
// using it says so in the operator's log exactly once.
func TestAzureRuntimeZeroValueClientIsNoRedirectsAndWarnsOnce(t *testing.T) {
	azureRuntime.mu.Lock()
	prevOpts, prevClient := azureRuntime.opts, azureRuntime.client
	prevConfigured, prevWarned := azureRuntime.configured, azureRuntime.warned
	azureRuntime.opts, azureRuntime.client = AzureFoundryOptions{}, unconfiguredAzureClient()
	azureRuntime.configured, azureRuntime.warned = false, false
	azureRuntime.mu.Unlock()
	t.Cleanup(func() {
		azureRuntime.mu.Lock()
		azureRuntime.opts, azureRuntime.client = prevOpts, prevClient
		azureRuntime.configured, azureRuntime.warned = prevConfigured, prevWarned
		azureRuntime.mu.Unlock()
	})

	client := azureHTTPClient()
	tr, ok := client.Transport.(*http.Transport)
	if !ok || tr == sharedHTTPClient.Transport {
		t.Fatalf("zero-value transport = %T, want a clone of the shared transport (local-mode posture)", client.Transport)
	}
	if tr.ResponseHeaderTimeout != azureResponseHeaderTimeout {
		t.Errorf("zero-value transport ResponseHeaderTimeout = %v, want %v", tr.ResponseHeaderTimeout, azureResponseHeaderTimeout)
	}
	if tr.DialContext != nil {
		t.Error("the zero-value transport installs no dial guard")
	}
	if client.CheckRedirect == nil || !errors.Is(client.CheckRedirect(nil, nil), http.ErrUseLastResponse) {
		t.Error("the zero-value client must refuse redirects")
	}
	if !azureRuntime.warned {
		t.Error("the first unconfigured use must set the warn-once flag")
	}
	azureHTTPClient()
	azureRuntime.mu.RLock()
	warned := azureRuntime.warned
	azureRuntime.mu.RUnlock()
	if !warned {
		t.Error("the warn flag must stay set (the log fires once, not per request)")
	}
}

// F-09: serverless *.models.ai.azure.com endpoints are refused with the
// migration message even when an operator lists the zone in
// PAD_AZURE_FOUNDRY_ALLOWED_HOSTS — the provider only speaks /openai/v1,
// which those endpoints never serve.
func TestAzureFoundry_ServerlessModelsHostRefusedEvenWhenAllowListed(t *testing.T) {
	cfg := AzureFoundryConfig{Endpoint: "https://phi-4-xyz.eastus2.models.ai.azure.com",
		Auth: AzureFoundryAuth{Mode: AzureAuthModeAPIKey, Key: "k"}, ChatDeployment: "phi-4"}
	cfg.normalize()
	for name, policy := range map[string]azureHostPolicy{
		"default policy":  {},
		"operator glob":   {extra: []string{"*.models.ai.azure.com"}},
		"operator exact ": {extra: []string{"phi-4-xyz.eastus2.models.ai.azure.com"}},
	} {
		err := cfg.Validate(policy)
		var cfgErr *AzureConfigError
		if !errors.As(err, &cfgErr) || cfgErr.Code != AzureCfgErrInvalidEndpoint {
			t.Errorf("%s: err = %v, want code %s", name, err, AzureCfgErrInvalidEndpoint)
			continue
		}
		if !strings.Contains(cfgErr.Message, "services.ai.azure.com") {
			t.Errorf("%s: message = %q, want the migration hint", name, cfgErr.Message)
		}
	}
}

// F-51: the Azure CLI sign-in mode (entra_cli) — the desktop user's own
// identity, no app registration, no secret.
func TestAzureFoundry_EntraCLIMode(t *testing.T) {
	valid := AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com",
		Auth: AzureFoundryAuth{Mode: AzureAuthModeEntraCLI}, ChatDeployment: "gpt-4o"}
	valid.normalize()

	saveAzureRuntime(t, AzureFoundryOptions{Egress: egress.AllowLoopback})
	if err := valid.Validate(azureHostPolicy{}); err != nil {
		t.Fatalf("desktop mode: %v", err)
	}
	withTenant := valid
	withTenant.Auth.TenantID = "contoso.onmicrosoft.com"
	if err := withTenant.Validate(azureHostPolicy{}); err != nil {
		t.Fatalf("with a tenant: %v", err)
	}
	badTenant := valid
	badTenant.Auth.TenantID = "common"
	if err := badTenant.Validate(azureHostPolicy{}); !isAzureCode(err, AzureCfgErrInvalidAuth) {
		t.Errorf("pseudo-tenant: %v", err)
	}
	withSecret := valid
	withSecret.Auth.ClientSecret = "s"
	if err := withSecret.Validate(azureHostPolicy{}); !isAzureCode(err, AzureCfgErrInvalidAuth) {
		t.Errorf("a secret in CLI mode: %v", err)
	}

	// A cloud deployment refuses the mode outright: a server-side `az login`
	// would be the operator's identity, not the user's.
	saveAzureRuntime(t, AzureFoundryOptions{Egress: egress.BlockInternal})
	err := valid.Validate(azureHostPolicy{})
	if !isAzureCode(err, AzureCfgErrInvalidAuth) || !strings.Contains(err.Error(), "desktop app only") {
		t.Errorf("cloud mode: %v", err)
	}
	if !valid.HasSecret() {
		t.Error("the CLI credential needs no stored secret, so HasSecret must hold")
	}
}

func isAzureCode(err error, code string) bool {
	var ce *AzureConfigError
	return errors.As(err, &ce) && ce.Code == code
}
