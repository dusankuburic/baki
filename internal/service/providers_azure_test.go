package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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
	// Finding #6: image, speech, transcription, realtime and video
	// deployments are never the chat default; flagged embedding entries are
	// never chosen at all (#42).
	nonChat := []ai.ModelInfo{{ID: "dall-e-3"}, {ID: "gpt-image-1"}, {ID: "whisper"}, {ID: "gpt-4o-mini-tts"}, {ID: "gpt-4o-transcribe"},
		{ID: "gpt-4o-realtime-preview"}, {ID: "sora"}, {ID: "vectors", Embedding: true}, {ID: "gpt-4o"}}
	if got := chooseDefaultModel("", nonChat, "gpt-4.1"); got != "gpt-4o" {
		t.Errorf("non-chat families must be skipped: %q", got)
	}
	if got := chooseDefaultModel("", []ai.ModelInfo{{ID: "vectors", Embedding: true}, {ID: "agent:Helper"}}, ""); got != "agent:Helper" {
		t.Errorf("a flagged embedding deployment is never the default: %q", got)
	}
	if got := chooseDefaultModel("", []ai.ModelInfo{{ID: "vectors", Embedding: true}}, ""); got != "" {
		t.Errorf("only an embedding deployment: %q, want no default", got)
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
	doc := `{"endpoint":"https://` + azureTestHost + `","auth":{"mode":"api_key","key":"k1"},"chatDeployment":"chat1",` +
		`"deployments":["gpt-4o"],"embeddingDeployment":"emb1"}`
	if err := secrets.Save("u1", "azure-foundry", doc); err != nil {
		t.Fatal(err)
	}
	svc := newProviderServiceWithFakeSecrets(t, secrets)

	providers, err := svc.ListProviders(context.Background(), "u1", "")
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
	// The listing is the document's own deployments (an API key cannot list
	// them); the chat deployment is the UI default, not the catalogue's.
	if az.DefaultModel != "chat1" {
		t.Errorf("default model = %q, want the configured deployment", az.DefaultModel)
	}
	// Finding #42: the embedding deployment is not offered as a chat model.
	var ids []string
	for _, m := range az.Models {
		ids = append(ids, m.ID)
	}
	if strings.Join(ids, ",") != "chat1,gpt-4o" {
		t.Errorf("models = %v, want the chat deployments without the embedding deployment", ids)
	}
	if gpt4o := az.Models[len(az.Models)-1]; gpt4o.InputCostPerM != 0 || gpt4o.OutputCostPerM != 0 || gpt4o.ContextLimit != 128000 {
		t.Errorf("catalogued deployment name = %+v, want the catalogue's window and no price (Azure is unpriced)", gpt4o)
	}
	// Alias providers cache their own listing (per document, complete only);
	// the service never pins it.
	if _, cached := svc.modelCache["u1\x00azure-foundry"]; cached {
		t.Error("the service must not cache an alias provider's listing")
	}

	providers, _ = svc.ListProviders(context.Background(), "u2", "")
	if az := findProvider(providers, "azure-foundry"); az == nil || az.Configured {
		t.Error("azure-foundry must not be configured for u2")
	}
}

// Finding #25: a document that names nothing lists nothing — and that empty
// listing is never cached, nor is the catalogue's "gpt-4.1" preselected.
func TestListProviders_AzureFoundry_EmptyListingNotCached(t *testing.T) {
	configureAzureForTests(t)
	secrets := testutil.NewFakeSecretStore()
	_ = secrets.Save("u1", "azure-foundry", `{"endpoint":"https://`+azureTestHost+`","auth":{"mode":"api_key","key":"k1"}}`)
	svc := newProviderServiceWithFakeSecrets(t, secrets)
	providers, err := svc.ListProviders(context.Background(), "u1", "")
	if err != nil {
		t.Fatal(err)
	}
	az := findProvider(providers, "azure-foundry")
	if az == nil || !az.Configured || az.Models == nil || len(az.Models) != 0 || az.DefaultModel != "" {
		t.Errorf("endpoint-only document = %+v, want configured with an empty (non-nil) model list and no default", az)
	}
	if _, cached := svc.modelCache["u1\x00azure-foundry"]; cached {
		t.Error("an empty listing must not be cached")
	}
}

// swapProvider replaces a registered provider constructor for one test.
func swapProvider(t *testing.T, id string, ctor, restore func(string) ai.Provider) {
	t.Helper()
	ai.RegisterProvider(id, ctor)
	t.Cleanup(func() { ai.RegisterProvider(id, restore) })
}

func restoreAzure(k string) ai.Provider { return ai.NewAzureFoundryProvider(k) }
func restoreGLM(k string) ai.Provider   { return ai.NewGLMProvider(k) }
func restoreXAI(k string) ai.Provider   { return ai.NewXAIProvider(k) }

// scriptedProvider is a provider whose listing, default and chat outcomes
// are scripted per test. models is called with the key (the metadata
// provider is built with ""), chat returns the error for a model (nil = ok).
type scriptedProvider struct {
	*testutil.FakeProvider
	key    string
	def    string
	free   string
	models func(ctx context.Context, key string) ([]ai.ModelInfo, error)
	chat   func(model string) error

	mu    sync.Mutex
	tried []string
}

func (p *scriptedProvider) Models(ctx context.Context) ([]ai.ModelInfo, error) {
	if p.models == nil {
		return nil, nil
	}
	return p.models(ctx, p.key)
}
func (p *scriptedProvider) DefaultModel() string { return p.def }
func (p *scriptedProvider) FreeModel() string    { return p.free }
func (p *scriptedProvider) Chat(_ context.Context, req ai.Request) (*ai.Response, error) {
	p.mu.Lock()
	p.tried = append(p.tried, req.Model)
	p.mu.Unlock()
	if p.chat != nil {
		if err := p.chat(req.Model); err != nil {
			return nil, err
		}
	}
	return &ai.Response{Content: "ok"}, nil
}

func (p *scriptedProvider) triedModels() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.tried...)
}

// Finding #6: for an alias provider the configured default always wins, even
// when the listing lacks it; without one, the first chat-capable listed entry
// (never a non-chat family, an embedding deployment or the catalogue name).
func TestListProviders_AliasDefaultWinsAndSkipsNonChat(t *testing.T) {
	def := "my-gpt4o"
	swapProvider(t, "azure-foundry", func(k string) ai.Provider {
		return &scriptedProvider{FakeProvider: &testutil.FakeProvider{}, key: k, def: def,
			models: func(_ context.Context, key string) ([]ai.ModelInfo, error) {
				if key == "" {
					return []ai.ModelInfo{{ID: "gpt-4.1"}}, nil
				}
				return []ai.ModelInfo{{ID: "dall-e-3"}, {ID: "vectors", Embedding: true}, {ID: "gpt-4o"}}, nil
			}}
	}, restoreAzure)
	secrets := testutil.NewFakeSecretStore()
	_ = secrets.Save("u1", "azure-foundry", "doc")
	svc := newProviderServiceWithFakeSecrets(t, secrets)

	providers, _ := svc.ListProviders(context.Background(), "u1", "")
	az := findProvider(providers, "azure-foundry")
	if az == nil || az.DefaultModel != "my-gpt4o" {
		t.Fatalf("default = %+v, want the configured deployment even though the listing lacks it", az)
	}
	for _, m := range az.Models {
		if m.ID == "vectors" {
			t.Error("embedding entry offered as a chat model")
		}
	}

	def = ""
	providers, _ = svc.ListProviders(context.Background(), "u1", "")
	if az := findProvider(providers, "azure-foundry"); az == nil || az.DefaultModel != "gpt-4o" {
		t.Errorf("no configured default: %+v, want the first chat-capable listed deployment", az)
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
	// A provider whose ids are model names and whose live listing lacks the
	// catalogue default: the listed chat model goes first (a default the
	// listing lacks is a guaranteed miss), so the test costs one call. The
	// configured-deployment-first order is Azure's (alias providers): see
	// TestTestProviderConnection_ConfiguredDefaultFirst.
	if strings.Join(lp.tried, ",") != "my-chat" {
		t.Errorf("tried models = %v, want the listed chat deployment first and alone", lp.tried)
	}
}

// Finding #6: the configured default (an Azure chat deployment the listing
// may lack) is tried first; model-level failures continue to the next
// candidate; the reported error is the default's; auth failures stop at once.
func TestTestProviderConnection_ConfiguredDefaultFirst(t *testing.T) {
	catalogue := func(context.Context, string) ([]ai.ModelInfo, error) {
		return []ai.ModelInfo{{ID: "dall-e-3-3.0"}, {ID: "vectors", Embedding: true}, {ID: "gpt-4o-2024-11-20"}}, nil
	}
	runAs := func(t *testing.T, id string, sp *scriptedProvider) (*scriptedProvider, string, bool) {
		t.Helper()
		secrets := testutil.NewFakeSecretStore()
		_ = secrets.Save("u1", id, "k")
		svc := newProviderServiceWithFakeSecrets(t, secrets)
		res, err := svc.TestProviderConnection(context.Background(), "u1", id)
		if err != nil {
			t.Fatal(err)
		}
		return sp, res.Error, res.Ok
	}
	// The configured-deployment-first order is the alias providers' (Azure AI
	// Foundry): the scripted provider stands in for it under its id.
	run := func(t *testing.T, sp *scriptedProvider) (*scriptedProvider, string, bool) {
		t.Helper()
		swapProvider(t, "azure-foundry", func(string) ai.Provider { return sp }, restoreAzure)
		return runAs(t, "azure-foundry", sp)
	}
	// runScripted registers the provider under a non-alias id of its own.
	runScripted := func(t *testing.T, sp *scriptedProvider) (*scriptedProvider, string, bool) {
		t.Helper()
		id := "scripted-" + strings.ReplaceAll(t.Name(), "/", "-")
		ai.RegisterProvider(id, func(string) ai.Provider { return sp })
		return runAs(t, id, sp)
	}

	t.Run("default missing from the listing still goes first", func(t *testing.T) {
		sp, _, ok := run(t, &scriptedProvider{FakeProvider: &testutil.FakeProvider{}, def: "my-gpt4o", models: catalogue,
			chat: func(m string) error {
				if m != "my-gpt4o" {
					return errors.New("azure ai foundry API: The API deployment for this resource does not exist.")
				}
				return nil
			}})
		if !ok || strings.Join(sp.triedModels(), ",") != "my-gpt4o" {
			t.Errorf("ok=%v tried=%v, want the configured deployment first and alone", ok, sp.triedModels())
		}
	})

	t.Run("model-level failures continue and report the default's error", func(t *testing.T) {
		sp, msg, ok := run(t, &scriptedProvider{FakeProvider: &testutil.FakeProvider{}, def: "typo", models: catalogue,
			chat: func(m string) error { return errors.New("deployment " + m + " not found") }})
		if ok || msg != "deployment typo not found" {
			t.Errorf("ok=%v error=%q, want the configured default's error", ok, msg)
		}
		// dall-e and the embedding entry are never connection-test candidates.
		if got := strings.Join(sp.triedModels(), ","); got != "typo,gpt-4o-2024-11-20" {
			t.Errorf("tried = %s", got)
		}
	})

	t.Run("a listed chat model rescues a missing default", func(t *testing.T) {
		sp, _, ok := run(t, &scriptedProvider{FakeProvider: &testutil.FakeProvider{}, def: "typo", models: catalogue,
			chat: func(m string) error {
				if m == "typo" {
					return errors.New("DeploymentNotFound")
				}
				return nil
			}})
		if !ok || strings.Join(sp.triedModels(), ",") != "typo,gpt-4o-2024-11-20" {
			t.Errorf("ok=%v tried=%v", ok, sp.triedModels())
		}
	})

	t.Run("an invalid key stops at once", func(t *testing.T) {
		sp, msg, ok := run(t, &scriptedProvider{FakeProvider: &testutil.FakeProvider{}, def: "typo", models: catalogue,
			chat: func(string) error { return ai.ErrApiKeyInvalid }})
		if ok || msg != "invalid API key" || len(sp.triedModels()) != 1 {
			t.Errorf("ok=%v error=%q tried=%v", ok, msg, sp.triedModels())
		}
	})

	// Finding #23: a principal without the Foundry User role gets Azure's
	// explanation (the principal and the missing data action), not "invalid
	// API key", and the test stops there.
	t.Run("a missing role reports the provider's explanation", func(t *testing.T) {
		denied := sentinelErr{msg: "permission denied: The principal `p1` lacks the required data action `Microsoft.CognitiveServices/accounts/OpenAI/deployments/chat/completions/action`",
			is: []error{ai.ErrProviderPermissionDenied, ai.ErrApiKeyInvalid}}
		sp, msg, ok := run(t, &scriptedProvider{FakeProvider: &testutil.FakeProvider{}, def: "typo", models: catalogue,
			chat: func(string) error { return denied }})
		if ok || !strings.Contains(msg, "lacks the required data action") || strings.Contains(msg, "invalid API key") || len(sp.triedModels()) != 1 {
			t.Errorf("ok=%v error=%q tried=%v", ok, msg, sp.triedModels())
		}
	})

	// Finding #15: rejected Entra ID credentials report Entra's one-line
	// reason with its AADSTS code.
	t.Run("rejected entra credentials report the AADSTS code", func(t *testing.T) {
		rejected := fmt.Errorf("get azure ai foundry token: %w", sentinelErr{msg: "Microsoft Entra ID rejected the tenant ID, client ID or client secret (AADSTS7000215 invalid_client)",
			is: []error{ai.ErrEntraCredentialsRejected, ai.ErrApiKeyInvalid}})
		sp, msg, ok := run(t, &scriptedProvider{FakeProvider: &testutil.FakeProvider{}, def: "typo", models: catalogue,
			chat: func(string) error { return rejected }})
		if ok || !strings.Contains(msg, "AADSTS7000215") || strings.Contains(msg, "invalid API key") || len(sp.triedModels()) != 1 {
			t.Errorf("ok=%v error=%q tried=%v", ok, msg, sp.triedModels())
		}
	})

	// D-11: a rejected Entra ID access token (the endpoint is not the
	// resource's own, or Entra ID authentication is off on it) reports
	// Azure's explanation and stops at once.
	t.Run("a rejected entra token reports azure's explanation", func(t *testing.T) {
		rejected := sentinelErr{msg: "azure ai foundry rejected the entra id access token: PermissionDenied: the token audience is not this resource",
			is: []error{ai.ErrEntraTokenRejected, ai.ErrApiKeyInvalid}}
		sp, msg, ok := run(t, &scriptedProvider{FakeProvider: &testutil.FakeProvider{}, def: "typo", models: catalogue,
			chat: func(string) error { return rejected }})
		if ok || !strings.Contains(msg, "token audience") || strings.Contains(msg, "invalid API key") || len(sp.triedModels()) != 1 {
			t.Errorf("ok=%v error=%q tried=%v", ok, msg, sp.triedModels())
		}
	})

	t.Run("nothing to test", func(t *testing.T) {
		// A non-Azure id: the generic message, no project probe.
		sp, msg, ok := runScripted(t, &scriptedProvider{FakeProvider: &testutil.FakeProvider{}})
		if ok || !strings.Contains(msg, "no model to test") || len(sp.triedModels()) != 0 {
			t.Errorf("ok=%v error=%q tried=%v, want a failure without any chat call", ok, msg, sp.triedModels())
		}
	})
}

// sentinelErr mirrors the ai package's auth errors that carry their own
// one-line text (a permission-denied answer, a refused Entra ID sign-in):
// the provider's message plus the sentinels it matches.
type sentinelErr struct {
	msg string
	is  []error
}

func (e sentinelErr) Error() string   { return e.msg }
func (e sentinelErr) Unwrap() []error { return e.is }

// Finding #25 / regressions#2: an empty live listing is cached only briefly
// (emptyListingTTL); a non-empty one for dynamicModelTTL.
func TestDynamicModels_CachesOnlyNonEmptyListings(t *testing.T) {
	var calls int32
	var empty atomic.Bool
	empty.Store(true)
	swapProvider(t, "glm", func(k string) ai.Provider {
		return &scriptedProvider{FakeProvider: &testutil.FakeProvider{}, key: k, def: "glm-5.2",
			models: func(_ context.Context, key string) ([]ai.ModelInfo, error) {
				if key == "" {
					return []ai.ModelInfo{{ID: "glm-5.2"}}, nil
				}
				atomic.AddInt32(&calls, 1)
				if empty.Load() {
					return []ai.ModelInfo{}, nil
				}
				return []ai.ModelInfo{{ID: "glm-5.2"}}, nil
			}}
	}, restoreGLM)
	secrets := testutil.NewFakeSecretStore()
	_ = secrets.Save("u1", "glm", "k")
	svc := newProviderServiceWithFakeSecrets(t, secrets)

	ctx := context.Background()
	start := time.Now()
	_, _, _ = svc.dynamicModels(ctx, personalScope("u1"), "glm")
	_, _, _ = svc.dynamicModels(ctx, personalScope("u1"), "glm")
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("empty listing was not cached briefly: %d upstream calls, want 1", n)
	}
	svc.modelMu.Lock()
	e := svc.modelCache["u1\x00glm"]
	svc.modelMu.Unlock()
	if e.expiresAt.After(start.Add(emptyListingTTL + time.Second)) {
		t.Errorf("empty listing expires at %v, want within %s", e.expiresAt, emptyListingTTL)
	}
	// The brief entry expired: the next call retries upstream.
	svc.modelMu.Lock()
	e.expiresAt = time.Now().Add(-time.Second)
	svc.modelCache["u1\x00glm"] = e
	svc.modelMu.Unlock()
	empty.Store(false)
	_, _, _ = svc.dynamicModels(ctx, personalScope("u1"), "glm")
	_, _, _ = svc.dynamicModels(ctx, personalScope("u1"), "glm")
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Errorf("non-empty listing not cached: %d upstream calls, want 2", n)
	}
	svc.modelMu.Lock()
	e = svc.modelCache["u1\x00glm"]
	svc.modelMu.Unlock()
	if e.expiresAt.Before(start.Add(dynamicModelTTL - time.Minute)) {
		t.Errorf("non-empty listing expires at %v, want about %s", e.expiresAt, dynamicModelTTL)
	}
}

// Finding #47: an InvalidateModels (a configuration save or key removal)
// while a listing for the old configuration is in flight keeps that listing
// out of the cache.
func TestDynamicModels_InvalidateDuringFetchSkipsTheWrite(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	swapProvider(t, "glm", func(k string) ai.Provider {
		return &scriptedProvider{FakeProvider: &testutil.FakeProvider{}, key: k, def: "glm-5.2",
			models: func(_ context.Context, key string) ([]ai.ModelInfo, error) {
				if key != "" {
					close(entered)
					<-release
				}
				return []ai.ModelInfo{{ID: "old-listing"}}, nil
			}}
	}, restoreGLM)
	secrets := testutil.NewFakeSecretStore()
	_ = secrets.Save("u1", "glm", "k")
	svc := newProviderServiceWithFakeSecrets(t, secrets)

	done := make(chan []ai.ModelInfo)
	go func() {
		m, _, _ := svc.dynamicModels(context.Background(), personalScope("u1"), "glm")
		done <- m
	}()
	<-entered
	svc.InvalidateModels("u1", "glm")
	close(release)
	if got := <-done; len(got) != 1 || got[0].ID != "old-listing" {
		t.Errorf("the in-flight caller still gets its listing: %+v", got)
	}
	svc.modelMu.Lock()
	_, cached := svc.modelCache["u1\x00glm"]
	svc.modelMu.Unlock()
	if cached {
		t.Error("a listing started before InvalidateModels was written back")
	}
}

// Finding #47: POST /api/keys/delete (SystemService.DeleteApiKey) runs the
// registered hook, which DI wires to ProviderService.InvalidateModels.
func TestDeleteApiKey_InvalidatesTheProviderListing(t *testing.T) {
	secrets := testutil.NewFakeSecretStore()
	_ = secrets.Save("u1", "azure-foundry", "doc")
	sys := NewSystemService(nil, secrets, nil, nil, "")
	svc := newProviderServiceWithFakeSecrets(t, secrets)
	svc.modelCache["u1\x00azure-foundry"] = modelCacheEntry{models: []ai.ModelInfo{{ID: "old"}}, expiresAt: time.Now().Add(time.Hour)}
	gen := svc.modelGen
	sys.OnApiKeyChanged(svc.InvalidateModels)
	if err := sys.DeleteApiKey("u1", "azure-foundry"); err != nil {
		t.Fatal(err)
	}
	if _, cached := svc.modelCache["u1\x00azure-foundry"]; cached || svc.modelGen == gen {
		t.Error("deleting the key must drop the cached listing and bump the generation")
	}
	if has, _ := secrets.Has("u1", "azure-foundry"); has {
		t.Error("key not deleted")
	}
}

// Finding #27: the configured providers' live listings run concurrently under
// a per-provider budget; one that hangs falls back to its metadata models
// without failing (or delaying past the budget) the whole listing, and the
// order is unchanged.
func TestListProviders_SlowProviderFallsBackWithinBudget(t *testing.T) {
	prev := liveModelsBudget
	liveModelsBudget = 200 * time.Millisecond
	t.Cleanup(func() { liveModelsBudget = prev })
	swapProvider(t, "xai", func(k string) ai.Provider {
		return &scriptedProvider{FakeProvider: &testutil.FakeProvider{}, key: k, def: "grok-3-mini",
			models: func(ctx context.Context, key string) ([]ai.ModelInfo, error) {
				if key == "" {
					return []ai.ModelInfo{{ID: "grok-3-mini", DisplayName: "metadata"}}, nil
				}
				<-ctx.Done() // a hung upstream
				return nil, ctx.Err()
			}}
	}, restoreXAI)
	swapProvider(t, "glm", func(k string) ai.Provider {
		return &scriptedProvider{FakeProvider: &testutil.FakeProvider{}, key: k, def: "glm-5.2",
			models: func(_ context.Context, key string) ([]ai.ModelInfo, error) {
				if key == "" {
					return []ai.ModelInfo{{ID: "glm-5.2"}}, nil
				}
				time.Sleep(5 * time.Second) // ignores its context altogether
				return []ai.ModelInfo{{ID: "late"}}, nil
			}}
	}, restoreGLM)
	secrets := testutil.NewFakeSecretStore()
	for _, id := range []string{"claude", "xai", "glm"} {
		_ = secrets.Save("u1", id, "k")
	}
	svc := newProviderServiceWithFakeSecrets(t, secrets)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	start := time.Now()
	providers, err := svc.ListProviders(ctx, "u1", "")
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	if elapsed > 2*time.Second {
		t.Errorf("ListProviders took %v, want about the per-provider budget", elapsed)
	}
	var order []string
	for _, p := range providers {
		order = append(order, p.ID)
	}
	var want []string
	for _, m := range ai.AvailableProviders() {
		if m.ID == ai.LocalProviderID {
			continue // listed only when configured (desktop)
		}
		want = append(want, m.ID)
	}
	if strings.Join(order, ",") != strings.Join(want, ",") {
		t.Errorf("order = %v, want %v", order, want)
	}
	if c := findProvider(providers, "claude"); c == nil || !c.Configured || len(c.Models) == 0 {
		t.Errorf("claude = %+v, want its listing despite the slow providers", c)
	}
	for _, id := range []string{"xai", "glm"} {
		p := findProvider(providers, id)
		if p == nil || !p.Configured || len(p.Models) != 1 || p.Models[0].ID == "late" {
			t.Errorf("%s = %+v, want the metadata fallback", id, p)
		}
	}
}
