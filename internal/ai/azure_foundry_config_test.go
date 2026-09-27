package ai

import (
	"errors"
	"strings"
	"testing"
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
		{"foreign host", AzureFoundryConfig{Endpoint: "https://evil.example", Auth: apiKey}, AzureCfgErrHostNotAllowed},
		{"suffix without subdomain", AzureFoundryConfig{Endpoint: "https://openai.azure.com", Auth: apiKey}, AzureCfgErrHostNotAllowed},
		{"metadata ip", AzureFoundryConfig{Endpoint: "https://169.254.169.254", Auth: apiKey}, AzureCfgErrHostNotAllowed},
		{"loopback name", AzureFoundryConfig{Endpoint: "https://localhost", Auth: apiKey}, AzureCfgErrHostNotAllowed},
		{"project path wrong", AzureFoundryConfig{Endpoint: "https://my-res.services.ai.azure.com", Auth: entra, ProjectEndpoint: "https://my-res.services.ai.azure.com/projects/p"}, AzureCfgErrInvalidProject},
		{"project foreign host", AzureFoundryConfig{Endpoint: "https://my-res.services.ai.azure.com", Auth: entra, ProjectEndpoint: "https://evil.example/api/projects/p"}, AzureCfgErrHostNotAllowed},
		{"api key missing", AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", Auth: AzureFoundryAuth{Mode: AzureAuthModeAPIKey}}, AzureCfgErrMissingSecret},
		{"entra secret missing", AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra, TenantID: entra.TenantID, ClientID: entra.ClientID}}, AzureCfgErrMissingSecret},
		{"entra tenant not a guid", AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", Auth: AzureFoundryAuth{Mode: AzureAuthModeEntra, TenantID: "contoso", ClientID: entra.ClientID, ClientSecret: "s"}}, AzureCfgErrInvalidAuth},
		{"unknown mode", AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", Auth: AzureFoundryAuth{Mode: "managed"}}, AzureCfgErrInvalidAuth},
		{"bad deployment name", AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", Auth: apiKey, ChatDeployment: "my deployment!"}, AzureCfgErrInvalidDeploy},
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
	if a.Fingerprint() == b.Fingerprint() {
		t.Error("a rotated key must change the fingerprint")
	}
	if a.Fingerprint() != c.Fingerprint() {
		t.Error("a deployment name must not change the fingerprint")
	}
}

func TestAzureHostPolicy(t *testing.T) {
	p := azureHostPolicy{extra: []string{"Gateway.Example.com", "*.corp.example", ""}}
	for host, want := range map[string]bool{
		"my-res.openai.azure.com":            true,
		"my-res.services.ai.azure.com":       true,
		"my-res.cognitiveservices.azure.com": true,
		"gateway.example.com":                true,
		"api.corp.example":                   true,
		"corp.example":                       false,
		"openai.azure.com":                   false,
		"evil.example":                       false,
		"127.0.0.1":                          false,
		"my-res.openai.azure.com.evil.com":   false,
	} {
		if got := p.allows(host); got != want {
			t.Errorf("allows(%q) = %v, want %v", host, got, want)
		}
	}
}
