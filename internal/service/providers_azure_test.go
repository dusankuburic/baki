package service

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/testutil"
)

// azureTestHost is allow-listed for the tests below and can never resolve
// (RFC 6761 reserves .invalid), so the provider's live listing fails fast and
// deterministically without any network traffic.
const azureTestHost = "unit-test.invalid"

const azureTestDoc = `{"endpoint":"https://` + azureTestHost + `","auth":{"mode":"api_key","key":"k1"},"chatDeployment":"chat1"}`

func configureAzureForTests(t *testing.T) {
	t.Helper()
	ai.ConfigureAzureFoundry(ai.AzureFoundryOptions{Egress: egress.AllowLoopback, ExtraAllowedHosts: []string{azureTestHost}})
	ai.ResetAzureFoundryCaches()
	t.Cleanup(func() { ai.ConfigureAzureFoundry(ai.AzureFoundryOptions{}) })
}

func TestChooseDefaultModel(t *testing.T) {
	models := []ai.ModelInfo{{ID: "text-embedding-3-small"}, {ID: "agent:Helper"}, {ID: "my-chat"}, {ID: "other"}}
	if got := chooseDefaultModel("other", models, "gpt-4.1"); got != "other" {
		t.Errorf("preferred present: %q", got)
	}
	if got := chooseDefaultModel("missing", models, "my-chat"); got != "my-chat" {
		t.Errorf("fallback present: %q", got)
	}
	if got := chooseDefaultModel("missing", models, "gpt-4.1"); got != "my-chat" {
		t.Errorf("first chat-capable entry (not the embedding deployment, not the agent): %q", got)
	}
	if got := chooseDefaultModel("", []ai.ModelInfo{{ID: "text-embedding-3-small"}}, ""); got != "text-embedding-3-small" {
		t.Errorf("only entry wins when nothing better exists: %q", got)
	}
	if got := chooseDefaultModel("", nil, "gpt-4.1"); got != "gpt-4.1" {
		t.Errorf("empty listing keeps the fallback: %q", got)
	}
}

func TestAzureFoundryConfig_SaveGetAndSecretMerge(t *testing.T) {
	secrets := testutil.NewFakeSecretStore()
	svc := newProviderServiceWithFakeSecrets(t, secrets)

	pub, err := svc.GetAzureFoundryConfig("u1")
	if err != nil || pub.Configured {
		t.Fatalf("unconfigured: %+v %v", pub, err)
	}

	cfg := ai.AzureFoundryConfig{Endpoint: "https://My-Res.openai.azure.com/", Auth: ai.AzureFoundryAuth{Mode: "api_key", Key: "k1"}, ChatDeployment: "chat1"}
	if err := svc.SaveAzureFoundryConfig("u1", cfg); err != nil {
		t.Fatalf("save: %v", err)
	}
	pub, _ = svc.GetAzureFoundryConfig("u1")
	if !pub.Configured || !pub.HasSecret || pub.Endpoint != "https://my-res.openai.azure.com" || pub.ChatDeployment != "chat1" || pub.AuthMode != "api_key" {
		t.Errorf("public view = %+v", pub)
	}

	// Editing without the secret keeps the stored one.
	edit := ai.AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", Auth: ai.AzureFoundryAuth{Mode: "api_key"}, ChatDeployment: "chat2"}
	if err := svc.SaveAzureFoundryConfig("u1", edit); err != nil {
		t.Fatalf("edit: %v", err)
	}
	raw, _ := secrets.Get("u1", "azure-foundry")
	stored, _ := ai.ParseAzureFoundryConfig(raw)
	if stored.Auth.Key != "k1" || stored.ChatDeployment != "chat2" {
		t.Errorf("stored after edit = %+v", stored)
	}

	// Switching auth mode needs a fresh secret.
	sw := ai.AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", Auth: ai.AzureFoundryAuth{Mode: "entra", TenantID: "11111111-2222-3333-4444-555555555555", ClientID: "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"}}
	err = svc.SaveAzureFoundryConfig("u1", sw)
	var cfgErr *ai.AzureConfigError
	if !errors.As(err, &cfgErr) || cfgErr.Code != ai.AzureCfgErrMissingSecret {
		t.Errorf("mode switch without secret: %v", err)
	}

	// Validation failures never overwrite the stored document.
	bad := ai.AzureFoundryConfig{Endpoint: "https://evil.example", Auth: ai.AzureFoundryAuth{Mode: "api_key", Key: "k9"}}
	err = svc.SaveAzureFoundryConfig("u1", bad)
	if !errors.As(err, &cfgErr) || cfgErr.Code != ai.AzureCfgErrHostNotAllowed {
		t.Errorf("host not allowed: %v", err)
	}
	raw, _ = secrets.Get("u1", "azure-foundry")
	if stored, _ := ai.ParseAzureFoundryConfig(raw); stored.Endpoint != "https://my-res.openai.azure.com" {
		t.Errorf("stored document changed by a rejected save: %+v", stored)
	}

	// Another user sees nothing.
	if pub, _ := svc.GetAzureFoundryConfig("u2"); pub.Configured {
		t.Error("configuration leaked across users")
	}
}

func TestListProviders_AzureFoundry_ConfiguredPerUser(t *testing.T) {
	configureAzureForTests(t)
	secrets := testutil.NewFakeSecretStore()
	if err := secrets.Save("u1", "azure-foundry", azureTestDoc); err != nil {
		t.Fatal(err)
	}
	svc := newProviderServiceWithFakeSecrets(t, secrets)

	providers, err := svc.ListProviders(context.Background(), "u1")
	if err != nil {
		t.Fatal(err)
	}
	az := findProvider(providers, "azure-foundry")
	if az == nil {
		t.Fatal("azure-foundry missing from the listing")
	}
	if !az.Configured || az.AuthType != "api_key" || az.Name != "Azure AI Foundry" {
		t.Errorf("azure-foundry entry = %+v", az)
	}
	// The live listing cannot be fetched (the host never resolves); the
	// provider falls back to the deployment named in the document, which
	// must become the UI default instead of the catalogue's.
	if az.DefaultModel != "chat1" {
		t.Errorf("default model = %q, want the configured deployment", az.DefaultModel)
	}
	if len(az.Models) != 1 || az.Models[0].ID != "chat1" {
		t.Errorf("models = %+v, want just the configured deployment", az.Models)
	}

	providers, _ = svc.ListProviders(context.Background(), "u2")
	if az := findProvider(providers, "azure-foundry"); az == nil || az.Configured {
		t.Error("azure-foundry must not be configured for u2")
	}
}

// listingProvider is a provider whose Models() listing does not contain its
// DefaultModel, the shape of a user-named Azure resource.
type listingProvider struct {
	*testutil.FakeProvider
	tried []string
}

func (p *listingProvider) Models(context.Context) ([]ai.ModelInfo, error) {
	return []ai.ModelInfo{{ID: "text-embedding-3-small", ContextLimit: 8191}, {ID: "agent:Helper", ContextLimit: 1}, {ID: "my-chat", ContextLimit: 128000}}, nil
}

func (p *listingProvider) DefaultModel() string { return "gpt-4.1" }

func (p *listingProvider) Chat(_ context.Context, req ai.Request) (*ai.Response, error) {
	p.tried = append(p.tried, req.Model)
	if req.Model != "my-chat" {
		return nil, errors.New("DeploymentNotFound")
	}
	return &ai.Response{Content: "ok"}, nil
}

func TestTestProviderConnection_FallsBackToListedModel(t *testing.T) {
	lp := &listingProvider{FakeProvider: &testutil.FakeProvider{}}
	ai.RegisterProvider("listing-fake", func(string) ai.Provider { return lp })
	secrets := testutil.NewFakeSecretStore()
	_ = secrets.Save("u1", "listing-fake", "k")
	svc := newProviderServiceWithFakeSecrets(t, secrets)

	res, err := svc.TestProviderConnection(context.Background(), "u1", "listing-fake")
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ok {
		t.Errorf("connection test failed: %+v", res)
	}
	if len(lp.tried) == 0 || lp.tried[0] != "my-chat" {
		t.Errorf("tried models = %v, want the listed chat deployment first", lp.tried)
	}
}
