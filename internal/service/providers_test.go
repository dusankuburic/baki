package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

func newProviderServiceWithFakeSecrets(t *testing.T, secrets *testutil.FakeSecretStore) *ProviderService {
	t.Helper()
	getKey := func(scope, provider string) (string, error) {
		v, _ := secrets.Get(scope, provider)
		return v, nil
	}
	factory := ai.NewProviderFactory(getKey, ai.NewCopilotAuth(), nil, nil)
	return NewProviderService(ai.NewGitHubAuth(), ai.NewCopilotAuth(), factory, secrets)
}

// setLocalModelsEgress installs the local models egress policy for the test
// and restores the unconfigured (cloud) posture afterwards.
func setLocalModelsEgress(t *testing.T, p egress.Policy) {
	t.Helper()
	ai.ConfigureLocalModels(ai.LocalModelsOptions{Egress: p})
	t.Cleanup(func() { ai.ConfigureLocalModels(ai.LocalModelsOptions{}) })
}

// B3: where local models are unavailable (cloud) the read is refused with the
// save's LOCAL_MODELS_UNAVAILABLE error, even over a stored document; on the
// desktop it answers the public view.
func TestGetLocalConfig_RefusedWhereUnavailable(t *testing.T) {
	secrets := testutil.NewFakeSecretStore()
	svc := newProviderServiceWithFakeSecrets(t, secrets)
	if err := secrets.Save("u1", ai.LocalProviderID, `{"baseUrl":"http://127.0.0.1:11434","contextWindow":16384}`); err != nil {
		t.Fatal(err)
	}

	setLocalModelsEgress(t, egress.BlockInternal)
	cfg, err := svc.GetLocalConfig("u1")
	var ce *ai.LocalConfigError
	if !errors.As(err, &ce) || ce.Code != ai.LocalCfgErrUnavailable || cfg.Configured {
		t.Errorf("cloud: cfg = %+v, err = %v, want LOCAL_MODELS_UNAVAILABLE", cfg, err)
	}

	setLocalModelsEgress(t, egress.AllowLoopback)
	cfg, err = svc.GetLocalConfig("u1")
	if err != nil || !cfg.Configured || cfg.BaseURL != "http://127.0.0.1:11434" || cfg.ContextWindow != 16384 {
		t.Errorf("desktop: cfg = %+v, err = %v", cfg, err)
	}
	if cfg, err := svc.GetLocalConfig("u2"); err != nil || cfg.Configured {
		t.Errorf("desktop, nothing stored: cfg = %+v, err = %v", cfg, err)
	}
}

// B11: the context window is validated and stored with the base URL; a
// refused window stores nothing.
func TestSaveLocalConfig_ContextWindow(t *testing.T) {
	setLocalModelsEgress(t, egress.AllowLoopback)
	secrets := testutil.NewFakeSecretStore()
	svc := newProviderServiceWithFakeSecrets(t, secrets)

	_, err := svc.SaveLocalConfig("u1", "http://127.0.0.1:11434", 512)
	var ce *ai.LocalConfigError
	if !errors.As(err, &ce) || ce.Code != ai.LocalCfgErrInvalidContextWindow {
		t.Errorf("512: err = %v, want INVALID_CONTEXT_WINDOW", err)
	}
	if has, _ := secrets.Has("u1", ai.LocalProviderID); has {
		t.Fatal("a refused save stored a document")
	}

	cfg, err := svc.SaveLocalConfig("u1", "http://127.0.0.1:11434/v1", 65536)
	if err != nil || cfg.ContextWindow != 65536 || cfg.BaseURL != "http://127.0.0.1:11434" {
		t.Fatalf("save = %+v, %v", cfg, err)
	}
	pub, err := svc.GetLocalConfig("u1")
	if err != nil || pub.ContextWindow != 65536 {
		t.Errorf("GET after save = %+v, %v", pub, err)
	}
}

// TestListProviders_GithubModels_PerUserConfigured verifies that a github-models
// OAuth token stored under a user's scope marks the provider configured for that
// user only — not for other users or the global scope.
func TestListProviders_GithubModels_PerUserConfigured(t *testing.T) {
	secrets := testutil.NewFakeSecretStore()
	if err := secrets.Save("u1", "github-models-token", "gho_u1"); err != nil {
		t.Fatal(err)
	}
	svc := newProviderServiceWithFakeSecrets(t, secrets)

	providers, err := svc.ListProviders(context.Background(), "u1", "")
	if err != nil {
		t.Fatalf("ListProviders(u1): %v", err)
	}
	if gm := findProvider(providers, "github-models"); gm == nil {
		t.Fatal("github-models provider missing from list")
	} else if !gm.Configured {
		t.Error("github-models should be Configured for u1 (per-user token stored)")
	}

	// A different user must NOT see u1's token as configured.
	providers, _ = svc.ListProviders(context.Background(), "u2", "")
	if gm := findProvider(providers, "github-models"); gm != nil && gm.Configured {
		t.Error("github-models should NOT be Configured for u2 (token leaked across users)")
	}
}

// TestListProviders_Copilot_PerUserConfigured verifies the copilot OAuth token is
// resolved per-user.
func TestListProviders_Copilot_PerUserConfigured(t *testing.T) {
	secrets := testutil.NewFakeSecretStore()
	if err := secrets.Save("u1", "copilot-oauth-token", "gho_u1"); err != nil {
		t.Fatal(err)
	}
	svc := newProviderServiceWithFakeSecrets(t, secrets)

	providers, err := svc.ListProviders(context.Background(), "u1", "")
	if err != nil {
		t.Fatalf("ListProviders(u1): %v", err)
	}
	if c := findProvider(providers, "copilot"); c == nil {
		t.Fatal("copilot provider missing from list")
	} else if !c.Configured {
		t.Error("copilot should be Configured for u1 (per-user OAuth token stored)")
	}

	providers, _ = svc.ListProviders(context.Background(), "u2", "")
	if c := findProvider(providers, "copilot"); c != nil && c.Configured {
		t.Error("copilot should NOT be Configured for u2 (OAuth token leaked across users)")
	}
}

func findProvider(ps []models.ProviderInfo, id string) *models.ProviderInfo {
	for i := range ps {
		if ps[i].ID == id {
			return &ps[i]
		}
	}
	return nil
}

// TestSaveAzureFoundryConfig_SecretCarryOver pins the blank-secret carry-over:
// the stored secret survives an edit that keeps the same credential (an API
// key, or the same Entra ID service principal), but rotating to a different
// principal must not silently attach the old principal's secret — a missing
// secret then fails validation with the same code as any missing secret
// (AADSTS7000215 only later, at first use, was the old behavior).
func TestSaveAzureFoundryConfig_SecretCarryOver(t *testing.T) {
	const tenant = "11111111-2222-3333-4444-555555555555"
	const client = "aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
	entra := func(tenantID, clientID string) ai.AzureFoundryConfig {
		return ai.AzureFoundryConfig{
			Endpoint:       "https://my-res.openai.azure.com",
			ChatDeployment: "chat1",
			Auth:           ai.AzureFoundryAuth{Mode: ai.AzureAuthModeEntra, TenantID: tenantID, ClientID: clientID},
		}
	}

	t.Run("same principal keeps the stored secret", func(t *testing.T) {
		secrets := testutil.NewFakeSecretStore()
		svc := newProviderServiceWithFakeSecrets(t, secrets)
		stored := entra(tenant, client)
		stored.Auth.ClientSecret = "old-secret"
		if err := svc.SaveAzureFoundryConfig("u1", stored); err != nil {
			t.Fatal(err)
		}
		// Same principal, new deployment name, blank secret.
		edited := entra(tenant, client)
		edited.ChatDeployment = "chat2"
		if err := svc.SaveAzureFoundryConfig("u1", edited); err != nil {
			t.Fatalf("blank secret with the same principal must carry over: %v", err)
		}
		raw, err := secrets.Get("u1", ai.AzureFoundryProviderID)
		if err != nil {
			t.Fatal(err)
		}
		parsed, err := ai.ParseAzureFoundryConfig(raw)
		if err != nil {
			t.Fatal(err)
		}
		if parsed.Auth.ClientSecret != "old-secret" || parsed.ChatDeployment != "chat2" {
			t.Errorf("stored document = %+v, want the carried secret and the new deployment", parsed)
		}
	})

	for name, rotated := range map[string]ai.AzureFoundryConfig{
		"rotated client id": entra(tenant, "99999999-8888-7777-6666-555555555555"),
		"rotated tenant id": entra("99999999-8888-7777-6666-555555555555", client),
	} {
		t.Run(name+" with a blank secret is refused", func(t *testing.T) {
			secrets := testutil.NewFakeSecretStore()
			svc := newProviderServiceWithFakeSecrets(t, secrets)
			stored := entra(tenant, client)
			stored.Auth.ClientSecret = "old-secret"
			if err := svc.SaveAzureFoundryConfig("u1", stored); err != nil {
				t.Fatal(err)
			}
			err := svc.SaveAzureFoundryConfig("u1", rotated)
			code := azureCfgErrCode(t, err)
			if code != "MISSING_SECRET" {
				t.Errorf("saving a new principal without a secret: code %q, want MISSING_SECRET (%v)", code, err)
			}
			// The stored document is untouched — the old secret did not
			// silently move to the new principal.
			raw, _ := secrets.Get("u1", ai.AzureFoundryProviderID)
			parsed, perr := ai.ParseAzureFoundryConfig(raw)
			if perr != nil || parsed.Auth.ClientID != client || parsed.Auth.ClientSecret != "old-secret" {
				t.Errorf("stored document changed under a refused save: %+v (%v)", parsed, perr)
			}
		})
	}

	t.Run("api key carries on the same endpoint only", func(t *testing.T) {
		secrets := testutil.NewFakeSecretStore()
		svc := newProviderServiceWithFakeSecrets(t, secrets)
		if err := svc.SaveAzureFoundryConfig("u1", ai.AzureFoundryConfig{
			Endpoint: "https://my-res.openai.azure.com", ChatDeployment: "chat1",
			Auth: ai.AzureFoundryAuth{Mode: ai.AzureAuthModeAPIKey, Key: "k1"},
		}); err != nil {
			t.Fatal(err)
		}
		edited := ai.AzureFoundryConfig{
			Endpoint: "https://my-res.openai.azure.com", ChatDeployment: "chat2",
			Auth: ai.AzureFoundryAuth{Mode: ai.AzureAuthModeAPIKey},
		}
		if err := svc.SaveAzureFoundryConfig("u1", edited); err != nil {
			t.Fatalf("blank api key on the same endpoint must carry over: %v", err)
		}
		raw, _ := secrets.Get("u1", ai.AzureFoundryProviderID)
		parsed, err := ai.ParseAzureFoundryConfig(raw)
		if err != nil || parsed.Auth.Key != "k1" || parsed.ChatDeployment != "chat2" {
			t.Errorf("stored document = %+v (%v), want the carried key and the new deployment", parsed, err)
		}
		// R-8: pointing the stored key at another host needs it re-entered.
		moved := edited
		moved.Endpoint = "https://my-other-res.openai.azure.com"
		err = svc.SaveAzureFoundryConfig("u1", moved)
		if code := azureCfgErrCode(t, err); code != ai.AzureCfgErrMissingSecret || !strings.Contains(err.Error(), "endpoint changed") {
			t.Errorf("blank api key on another endpoint: code %q (%v), want MISSING_SECRET explaining why", code, err)
		}
		raw, _ = secrets.Get("u1", ai.AzureFoundryProviderID)
		if parsed, _ = ai.ParseAzureFoundryConfig(raw); parsed.Endpoint != edited.Endpoint {
			t.Errorf("a refused save moved the stored document: %+v", parsed)
		}
	})
}

func azureCfgErrCode(t *testing.T, err error) string {
	t.Helper()
	if err == nil {
		return ""
	}
	var ce *ai.AzureConfigError
	if !errors.As(err, &ce) {
		t.Fatalf("error is not an *ai.AzureConfigError: %v", err)
	}
	return ce.Code
}

// seedModelCache writes entries directly (the map is the unit under test).
func seedModelCache(svc *ProviderService, entries map[string]modelCacheEntry) {
	svc.modelMu.Lock()
	defer svc.modelMu.Unlock()
	for k, e := range entries {
		svc.modelCache[k] = e
	}
}

func modelCacheKeys(svc *ProviderService) map[string]bool {
	svc.modelMu.Lock()
	defer svc.modelMu.Unlock()
	out := make(map[string]bool, len(svc.modelCache))
	for k := range svc.modelCache {
		out[k] = true
	}
	return out
}

// TestPruneModelCache_RemovesExpiredKeepsLiveAndCapsSize pins the insert-time
// pruning: expired entries go first (nothing else ever removes them, so a
// departed user's keys would otherwise accumulate forever), and at
// modelCacheMax the entry closest to expiry is dropped to make room.
func TestPruneModelCache_RemovesExpiredKeepsLiveAndCapsSize(t *testing.T) {
	svc := newProviderServiceWithFakeSecrets(t, testutil.NewFakeSecretStore())
	now := time.Now()
	seedModelCache(svc, map[string]modelCacheEntry{
		"u1\x00glm":     {models: []ai.ModelInfo{{ID: "expired"}}, expiresAt: now.Add(-time.Minute)},
		"u1\x00copilot": {models: []ai.ModelInfo{{ID: "also-expired"}}, expiresAt: now},
		"u2\x00glm":     {models: []ai.ModelInfo{{ID: "live"}}, expiresAt: now.Add(time.Hour)},
	})

	svc.pruneModelCacheLocked(now)

	keys := modelCacheKeys(svc)
	if keys["u1\x00glm"] || keys["u1\x00copilot"] {
		t.Errorf("expired entries survived pruning: %v", keys)
	}
	if !keys["u2\x00glm"] {
		t.Errorf("live entry was pruned: %v", keys)
	}

	// Cap: a full map of live entries evicts nearest-to-expiry on the way in.
	full := map[string]modelCacheEntry{}
	for i := 0; i < modelCacheMax; i++ {
		full[fmt.Sprintf("user%d\x00glm", i)] = modelCacheEntry{
			models:    []ai.ModelInfo{{ID: "m"}},
			expiresAt: now.Add(time.Duration(i+1) * time.Hour),
		}
	}
	svc.modelMu.Lock()
	svc.modelCache = map[string]modelCacheEntry{}
	for k, v := range full {
		svc.modelCache[k] = v
	}
	svc.pruneModelCacheLocked(now)
	size := len(svc.modelCache)
	svc.modelMu.Unlock()
	if size != modelCacheMax-1 {
		t.Errorf("full-map prune left %d entries, want %d (one eviction to make room)", size, modelCacheMax-1)
	}
}

// TestDynamicModels_InsertPrunesExpiredEntries verifies the wiring: a
// cacheable insert runs the pruning, so entries that expired in place —
// invisible to reads but never removed before — are dropped alongside it.
func TestDynamicModels_InsertPrunesExpiredEntries(t *testing.T) {
	swapProvider(t, "glm", func(k string) ai.Provider {
		return &scriptedProvider{FakeProvider: &testutil.FakeProvider{}, key: k, def: "glm-5.2",
			models: func(_ context.Context, key string) ([]ai.ModelInfo, error) {
				if key == "" {
					return []ai.ModelInfo{{ID: "glm-5.2"}}, nil
				}
				return []ai.ModelInfo{{ID: "glm-5.2"}}, nil
			}}
	}, restoreGLM)
	secrets := testutil.NewFakeSecretStore()
	_ = secrets.Save("u1", "glm", "k")
	svc := newProviderServiceWithFakeSecrets(t, secrets)
	seedModelCache(svc, map[string]modelCacheEntry{
		"gone-user\x00glm": {models: []ai.ModelInfo{{ID: "stale"}}, expiresAt: time.Now().Add(-time.Minute)},
	})

	if _, _, ok := svc.dynamicModels(context.Background(), personalScope("u1"), "glm"); !ok {
		t.Fatal("dynamicModels failed on the scripted provider")
	}

	keys := modelCacheKeys(svc)
	if keys["gone-user\x00glm"] {
		t.Errorf("expired entry survived an insert: %v", keys)
	}
	if !keys["u1\x00glm"] {
		t.Errorf("fresh listing was not cached: %v", keys)
	}
}

// TestInvalidateScope_RemovesOnlyThatScopesKeys: account deletion drops every
// listing of the erased user (including still-live ones and ones whose scope
// is a PREFIX of another user's — the \x00 separator must keep "u1" from
// matching "u10") and bumps the generation like InvalidateModels does.
func TestInvalidateScope_RemovesOnlyThatScopesKeys(t *testing.T) {
	svc := newProviderServiceWithFakeSecrets(t, testutil.NewFakeSecretStore())
	live := modelCacheEntry{models: []ai.ModelInfo{{ID: "m"}}, expiresAt: time.Now().Add(time.Hour)}
	seedModelCache(svc, map[string]modelCacheEntry{
		"u1\x00glm":     live,
		"u1\x00copilot": live,
		"u10\x00glm":    live,
		"u2\x00glm":     live,
	})
	svc.modelMu.Lock()
	gen := svc.modelGen
	svc.modelMu.Unlock()

	svc.InvalidateScope("u1")

	keys := modelCacheKeys(svc)
	if keys["u1\x00glm"] || keys["u1\x00copilot"] {
		t.Errorf("u1's listings survived the scope invalidation: %v", keys)
	}
	if !keys["u10\x00glm"] || !keys["u2\x00glm"] {
		t.Errorf("other scopes' listings were dropped: %v", keys)
	}
	svc.modelMu.Lock()
	bumped := svc.modelGen != gen
	svc.modelMu.Unlock()
	if !bumped {
		t.Error("InvalidateScope must bump the generation (in-flight listings must not write back)")
	}
}

// TestRevokeGitHubAuth_InvalidatesListing and the copilot twin: revoking the
// OAuth credential drops that provider's cached live listing immediately
// (github-models/copilot are cacheable listings), instead of after
// dynamicModelTTL.
func TestRevokeGitHubAuth_InvalidatesListing(t *testing.T) {
	secrets := testutil.NewFakeSecretStore()
	_ = secrets.Save("u1", "github-models-token", "gho_x")
	svc := newProviderServiceWithFakeSecrets(t, secrets)
	seedModelCache(svc, map[string]modelCacheEntry{
		"u1\x00github-models": {models: []ai.ModelInfo{{ID: "old"}}, expiresAt: time.Now().Add(time.Hour)},
	})
	svc.modelMu.Lock()
	gen := svc.modelGen
	svc.modelMu.Unlock()

	if err := svc.RevokeGitHubAuth("u1"); err != nil {
		t.Fatal(err)
	}

	if _, cached := modelCacheKeys(svc)["u1\x00github-models"]; cached {
		t.Error("revoking the token must drop the cached listing")
	}
	svc.modelMu.Lock()
	bumped := svc.modelGen != gen
	svc.modelMu.Unlock()
	if !bumped {
		t.Error("revoking the token must bump the generation")
	}
	if has, _ := secrets.Has("u1", "github-models-token"); has {
		t.Error("token not deleted")
	}
}

func TestRevokeCopilotAuth_InvalidatesListing(t *testing.T) {
	secrets := testutil.NewFakeSecretStore()
	_ = secrets.Save("u1", "copilot-oauth-token", "gho_x")
	svc := newProviderServiceWithFakeSecrets(t, secrets)
	seedModelCache(svc, map[string]modelCacheEntry{
		"u1\x00copilot": {models: []ai.ModelInfo{{ID: "old"}}, expiresAt: time.Now().Add(time.Hour)},
	})

	if err := svc.RevokeCopilotAuth("u1"); err != nil {
		t.Fatal(err)
	}

	if _, cached := modelCacheKeys(svc)["u1\x00copilot"]; cached {
		t.Error("revoking the token must drop the cached listing")
	}
	if has, _ := secrets.Has("u1", "copilot-oauth-token"); has {
		t.Error("token not deleted")
	}
}
