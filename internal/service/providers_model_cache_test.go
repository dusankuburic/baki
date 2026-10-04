package service

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/testutil"
)

// newFastRetryProviderService is newProviderServiceWithFakeSecrets with a
// retry wrapper whose backoff is negligible, so a scripted outage costs the
// test milliseconds instead of the production 0.5 s + 1 s.
func newFastRetryProviderService(t *testing.T, secrets *testutil.FakeSecretStore) *ProviderService {
	t.Helper()
	getKey := func(scope, provider string) (string, error) {
		v, _ := secrets.Get(scope, provider)
		return v, nil
	}
	rt := &config.RuntimeConfig{RetryMaxAttempts: 3, RetryBaseDelay: "1ms", CircuitBreakerFailures: 100, CircuitBreakerOpenDur: "1s"}
	factory := ai.NewProviderFactory(getKey, ai.NewCopilotAuth(), nil, rt)
	return NewProviderService(ai.NewGitHubAuth(), ai.NewCopilotAuth(), factory, secrets)
}

// D-14: an Entra ID + project document that names no deployment relies on
// the project listing; when that listing is empty the connection test
// reports the probe's explanation (a rejected secret, a missing role, a
// wrong project) instead of "name a chat deployment".
func TestTestProviderConnection_AzureEntraProjectFailureIsReported(t *testing.T) {
	sp := &scriptedProvider{FakeProvider: &testutil.FakeProvider{}}
	swapProvider(t, "azure-foundry", func(string) ai.Provider { return sp }, restoreAzure)
	prev := azureProjectProbe
	var probed atomic.Int32
	azureProjectProbe = func(_ context.Context, raw string) error {
		probed.Add(1)
		if raw != "entra-doc" {
			t.Errorf("probe got raw %q, want the stored document", raw)
		}
		return errors.New("Microsoft Entra ID rejected the tenant ID, client ID or client secret (AADSTS7000215 invalid_client)")
	}
	t.Cleanup(func() { azureProjectProbe = prev })

	secrets := testutil.NewFakeSecretStore()
	_ = secrets.Save("u1", "azure-foundry", "entra-doc")
	svc := newProviderServiceWithFakeSecrets(t, secrets)
	res, err := svc.TestProviderConnection(context.Background(), "u1", "azure-foundry")
	if err != nil {
		t.Fatal(err)
	}
	if res.Ok || !strings.Contains(res.Error, "AADSTS7000215") || strings.Contains(res.Error, "no model to test") {
		t.Errorf("result = %+v, want the probe's explanation", res)
	}
	if probed.Load() != 1 || len(sp.triedModels()) != 0 {
		t.Errorf("probed=%d tried=%v, want one probe and no chat call", probed.Load(), sp.triedModels())
	}

	t.Run("a healthy project keeps the generic message", func(t *testing.T) {
		azureProjectProbe = func(context.Context, string) error { return nil }
		res, err := svc.TestProviderConnection(context.Background(), "u1", "azure-foundry")
		if err != nil {
			t.Fatal(err)
		}
		if res.Ok || !strings.Contains(res.Error, "no model to test") {
			t.Errorf("result = %+v", res)
		}
	})
}

// D-53: an outage is not model-level — the next candidate would only repeat
// the same retries against the same failure — so the test stops at the
// first candidate and reports the outage.
func TestTestProviderConnection_StopsOnProviderOutage(t *testing.T) {
	catalogue := func(context.Context, string) ([]ai.ModelInfo, error) {
		return []ai.ModelInfo{{ID: "gpt-4o"}}, nil
	}
	for _, tc := range []struct {
		name    string
		err     error
		attempt int // chat calls, all for the default: retries only
		want    string
	}{
		{"provider down", fmt.Errorf("%w (status 503)", ai.ErrProviderDown), 3, ai.ErrProviderDown.Error()},
		{"circuit open", ai.ErrCircuitOpen, 1, ai.ErrCircuitOpen.Error()},
		{"config invalid", fmt.Errorf("%w: endpoint missing", ai.ErrProviderConfigInvalid), 1, "endpoint missing"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			sp := &scriptedProvider{FakeProvider: &testutil.FakeProvider{}, def: "typo", models: catalogue,
				chat: func(string) error { return tc.err }}
			swapProvider(t, "azure-foundry", func(string) ai.Provider { return sp }, restoreAzure)
			secrets := testutil.NewFakeSecretStore()
			_ = secrets.Save("u1", "azure-foundry", "k")
			svc := newFastRetryProviderService(t, secrets)
			res, err := svc.TestProviderConnection(context.Background(), "u1", "azure-foundry")
			if err != nil {
				t.Fatal(err)
			}
			if res.Ok || !strings.Contains(res.Error, tc.want) {
				t.Errorf("result = %+v, want the outage reported", res)
			}
			tried := sp.triedModels()
			if len(tried) != tc.attempt {
				t.Errorf("tried = %v, want %d call(s) for the default only", tried, tc.attempt)
			}
			for _, m := range tried {
				if m != "typo" {
					t.Errorf("a second candidate %q was tried during the outage", m)
				}
			}
		})
	}
}

// regressions#0: for a provider whose model ids are model names, a catalogue
// default the live listing lacks is a guaranteed miss — the listed model goes
// first so the test costs one call.
func TestTestProviderConnection_NonAliasListedModelFirst(t *testing.T) {
	sp := &scriptedProvider{FakeProvider: &testutil.FakeProvider{}, def: "gpt-4o",
		models: func(context.Context, string) ([]ai.ModelInfo, error) {
			return []ai.ModelInfo{{ID: "gpt-4o-mini"}}, nil
		},
		chat: func(m string) error {
			if m != "gpt-4o-mini" {
				return errors.New("model " + m + " not found")
			}
			return nil
		}}
	id := "scripted-non-alias-listed-first"
	ai.RegisterProvider(id, func(string) ai.Provider { return sp })
	secrets := testutil.NewFakeSecretStore()
	_ = secrets.Save("u1", id, "k")
	svc := newProviderServiceWithFakeSecrets(t, secrets)
	res, err := svc.TestProviderConnection(context.Background(), "u1", id)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Ok || strings.Join(sp.triedModels(), ",") != "gpt-4o-mini" {
		t.Errorf("ok=%v tried=%v, want the listed model first and alone", res.Ok, sp.triedModels())
	}

	t.Run("a default the listing has stays first", func(t *testing.T) {
		sp.tried = nil
		sp.models = func(context.Context, string) ([]ai.ModelInfo, error) {
			return []ai.ModelInfo{{ID: "gpt-4o-mini"}, {ID: "gpt-4o"}}, nil
		}
		sp.chat = nil
		res, err := svc.TestProviderConnection(context.Background(), "u1", id)
		if err != nil {
			t.Fatal(err)
		}
		if !res.Ok || strings.Join(sp.triedModels(), ",") != "gpt-4o" {
			t.Errorf("ok=%v tried=%v", res.Ok, sp.triedModels())
		}
	})
}

// regressions#2: an empty live listing is cached for emptyListingTTL — one
// upstream call per Settings open, not per provider list — and expires soon.
func TestDynamicModels_EmptyListingCachedBriefly(t *testing.T) {
	var calls int32
	swapProvider(t, "glm", func(k string) ai.Provider {
		return &scriptedProvider{FakeProvider: &testutil.FakeProvider{}, key: k, def: "glm-5.2",
			models: func(_ context.Context, key string) ([]ai.ModelInfo, error) {
				if key != "" {
					atomic.AddInt32(&calls, 1)
				}
				return []ai.ModelInfo{}, nil
			}}
	}, restoreGLM)
	secrets := testutil.NewFakeSecretStore()
	_ = secrets.Save("u1", "glm", "k")
	svc := newProviderServiceWithFakeSecrets(t, secrets)

	ctx := context.Background()
	before := time.Now()
	_, _, _ = svc.dynamicModels(ctx, personalScope("u1"), "glm")
	_, _, _ = svc.dynamicModels(ctx, personalScope("u1"), "glm")
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Errorf("empty listing was not cached: %d upstream calls, want 1", n)
	}
	svc.modelMu.Lock()
	e, cached := svc.modelCache["u1\x00glm"]
	svc.modelMu.Unlock()
	if !cached {
		t.Fatal("the empty listing is not in the cache")
	}
	if e.expiresAt.After(before.Add(emptyListingTTL+time.Second)) || len(e.models) != 0 {
		t.Errorf("entry = %+v, want an empty listing expiring within %s", e, emptyListingTTL)
	}
}

// D-54: an invalidation for another key or another scope does not fence a
// listing in flight — one user's Azure save must not discard another user's
// listing (and cost them a second upstream call).
func TestDynamicModels_OtherKeyInvalidationDoesNotFenceWriteBack(t *testing.T) {
	run := func(t *testing.T, invalidate func(svc *ProviderService)) {
		t.Helper()
		entered := make(chan struct{})
		release := make(chan struct{})
		swapProvider(t, "glm", func(k string) ai.Provider {
			return &scriptedProvider{FakeProvider: &testutil.FakeProvider{}, key: k, def: "glm-5.2",
				models: func(_ context.Context, key string) ([]ai.ModelInfo, error) {
					if key != "" {
						close(entered)
						<-release
					}
					return []ai.ModelInfo{{ID: "glm-5.2"}}, nil
				}}
		}, restoreGLM)
		secrets := testutil.NewFakeSecretStore()
		_ = secrets.Save("u2", "glm", "k")
		svc := newProviderServiceWithFakeSecrets(t, secrets)

		done := make(chan struct{})
		go func() {
			_, _, _ = svc.dynamicModels(context.Background(), personalScope("u2"), "glm")
			close(done)
		}()
		<-entered
		invalidate(svc)
		close(release)
		<-done
		svc.modelMu.Lock()
		_, cached := svc.modelCache["u2\x00glm"]
		svc.modelMu.Unlock()
		if !cached {
			t.Error("an invalidation for another key/scope fenced off the write-back")
		}
	}
	t.Run("other key", func(t *testing.T) {
		run(t, func(svc *ProviderService) { svc.InvalidateModels("u1", "azure-foundry") })
	})
	t.Run("other scope", func(t *testing.T) {
		run(t, func(svc *ProviderService) { svc.InvalidateScope("u1") })
	})
	t.Run("same scope still fences", func(t *testing.T) {
		entered := make(chan struct{})
		release := make(chan struct{})
		swapProvider(t, "glm", func(k string) ai.Provider {
			return &scriptedProvider{FakeProvider: &testutil.FakeProvider{}, key: k, def: "glm-5.2",
				models: func(_ context.Context, key string) ([]ai.ModelInfo, error) {
					if key != "" {
						close(entered)
						<-release
					}
					return []ai.ModelInfo{{ID: "glm-5.2"}}, nil
				}}
		}, restoreGLM)
		secrets := testutil.NewFakeSecretStore()
		_ = secrets.Save("u2", "glm", "k")
		svc := newProviderServiceWithFakeSecrets(t, secrets)
		done := make(chan struct{})
		go func() {
			_, _, _ = svc.dynamicModels(context.Background(), personalScope("u2"), "glm")
			close(done)
		}()
		<-entered
		svc.InvalidateScope("u2")
		close(release)
		<-done
		svc.modelMu.Lock()
		_, cached := svc.modelCache["u2\x00glm"]
		svc.modelMu.Unlock()
		if cached {
			t.Error("a listing started before InvalidateScope of its own scope was written back")
		}
	})
}

// D-54: stale invalidation marks are pruned with the cache so they do not
// accumulate one per key ever invalidated.
func TestPruneModelCache_DropsStaleInvalidationMarks(t *testing.T) {
	svc := newProviderServiceWithFakeSecrets(t, testutil.NewFakeSecretStore())
	svc.InvalidateModels("u1", "glm")
	svc.InvalidateScope("u2")
	svc.modelMu.Lock()
	svc.pruneModelCacheLocked(time.Now())
	kept := len(svc.invalidatedKeys) + len(svc.invalidatedScope)
	svc.pruneModelCacheLocked(time.Now().Add(invalidationFence + time.Second))
	stale := len(svc.invalidatedKeys) + len(svc.invalidatedScope)
	svc.modelMu.Unlock()
	if kept != 2 || stale != 0 {
		t.Errorf("marks kept=%d stale=%d, want 2 then 0", kept, stale)
	}
}

// Invalidations prune dead marks themselves: alias providers (Azure AI
// Foundry) never insert a cacheable listing, so in an Azure-only deployment
// the insert-time prune never runs and a mark per key save, org credential
// change and account deletion would accumulate for good.
func TestInvalidate_PrunesStaleMarksWithoutAnInsert(t *testing.T) {
	svc := newProviderServiceWithFakeSecrets(t, testutil.NewFakeSecretStore())
	for i := 0; i < 20; i++ {
		svc.InvalidateModels(fmt.Sprintf("u%d", i), "azure-foundry")
		svc.InvalidateScope(fmt.Sprintf("s%d", i))
	}
	// Age every mark past the fence, then invalidate again with no listing
	// inserted in between.
	svc.modelMu.Lock()
	for k, inv := range svc.invalidatedKeys {
		inv.at = inv.at.Add(-invalidationFence - time.Second)
		svc.invalidatedKeys[k] = inv
	}
	for k, inv := range svc.invalidatedScope {
		inv.at = inv.at.Add(-invalidationFence - time.Second)
		svc.invalidatedScope[k] = inv
	}
	svc.modelMu.Unlock()
	svc.InvalidateModels("fresh", "azure-foundry")
	svc.InvalidateScope("fresh-scope")

	svc.modelMu.Lock()
	keys, scopes := len(svc.invalidatedKeys), len(svc.invalidatedScope)
	svc.modelMu.Unlock()
	if keys != 1 || scopes != 1 {
		t.Errorf("marks after invalidating past the fence: keys=%d scopes=%d, want 1 and 1 (the fresh ones)", keys, scopes)
	}
}

// V-cfg-1: saving a replacement key drops the previous key's cached listing
// (the hook SaveApiKey shares with DeleteApiKey).
func TestSaveApiKey_InvalidatesTheProviderListing(t *testing.T) {
	secrets := testutil.NewFakeSecretStore()
	_ = secrets.Save("u1", "openai", "k1")
	sys := NewSystemService(nil, secrets, nil, nil, "")
	svc := newProviderServiceWithFakeSecrets(t, secrets)
	svc.modelCache["u1\x00openai"] = modelCacheEntry{models: []ai.ModelInfo{{ID: "old"}}, expiresAt: time.Now().Add(time.Hour)}
	gen := svc.modelGen
	sys.OnApiKeyChanged(svc.InvalidateModels)
	if err := sys.SaveApiKey("u1", "openai", "k2"); err != nil {
		t.Fatal(err)
	}
	if _, cached := svc.modelCache["u1\x00openai"]; cached || svc.modelGen == gen {
		t.Error("saving a replacement key must drop the cached listing and bump the generation")
	}
	if v, _ := secrets.Get("u1", "openai"); v != "k2" {
		t.Errorf("stored key = %q, want k2", v)
	}
}

// D-15: removing the Azure AI Foundry document releases what the process
// kept for it (ai.ForgetAzureFoundryCredential) and still runs the hook
// after the delete; a document that cannot be parsed is simply deleted.
func TestDeleteApiKey_ForgetsAzureCredential(t *testing.T) {
	cfg := ai.AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", ChatDeployment: "chat"}
	cfg.Auth.Mode = ai.AzureAuthModeAPIKey
	cfg.Auth.Key = "k1"
	cfg.Normalize()
	raw, err := cfg.Encode()
	if err != nil {
		t.Fatal(err)
	}
	for _, doc := range []string{raw, "not-a-document"} {
		secrets := testutil.NewFakeSecretStore()
		_ = secrets.Save("u1", "azure-foundry", doc)
		sys := NewSystemService(nil, secrets, nil, nil, "")
		var hooked []string
		sys.OnApiKeyChanged(func(scope, provider string) {
			if has, _ := secrets.Has(scope, provider); has {
				t.Error("the hook ran before the delete")
			}
			hooked = append(hooked, scope+"/"+provider)
		})
		if err := sys.DeleteApiKey("u1", "azure-foundry"); err != nil {
			t.Fatal(err)
		}
		if has, _ := secrets.Has("u1", "azure-foundry"); has {
			t.Error("document not deleted")
		}
		if strings.Join(hooked, ",") != "u1/azure-foundry" {
			t.Errorf("hook calls = %v", hooked)
		}
	}
}

// D-15: replacing the credential forgets the previous one (no panic, the
// carry-over still works, the new document is what is stored).
func TestSaveAzureFoundryConfig_ForgetsReplacedCredential(t *testing.T) {
	secrets := testutil.NewFakeSecretStore()
	svc := newProviderServiceWithFakeSecrets(t, secrets)
	doc := func(key string) ai.AzureFoundryConfig {
		cfg := ai.AzureFoundryConfig{Endpoint: "https://my-res.openai.azure.com", ChatDeployment: "chat"}
		cfg.Auth.Mode = ai.AzureAuthModeAPIKey
		cfg.Auth.Key = key
		return cfg
	}
	if err := svc.SaveAzureFoundryConfig("u1", doc("k1")); err != nil {
		t.Fatal(err)
	}
	if err := svc.SaveAzureFoundryConfig("u1", doc("k2")); err != nil { // rotated: k1 is forgotten
		t.Fatal(err)
	}
	if err := svc.SaveAzureFoundryConfig("u1", doc("")); err != nil { // edit without the key: k2 carried over
		t.Fatal(err)
	}
	raw, _ := secrets.Get("u1", "azure-foundry")
	stored, err := ai.ParseAzureFoundryConfig(raw)
	if err != nil {
		t.Fatal(err)
	}
	if stored.Auth.Key != "k2" {
		t.Errorf("stored key = %q, want the rotated k2", stored.Auth.Key)
	}
}
