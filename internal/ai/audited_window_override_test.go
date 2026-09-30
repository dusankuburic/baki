package ai

import (
	"context"
	"errors"
	"net/http"
	"testing"
)

// Part 9: the audited wrapper applies a registry resource's model entries to
// the listing — every registered entry is listed whatever the live listing
// returned, and the user's context-window and max-output overrides beat the
// catalogue, the project listing and the served-model memo, while the window
// a deployment's own rejection stated still clamps them.

// registryChain wraps a registry provider in the factory's chain as user u1.
func registryChain(t *testing.T, p *AzureFoundryProvider) (Provider, *auditedProvider) {
	t.Helper()
	chain := (&ProviderFactory{}).wrapChain(p, "u1", azureFoundryProviderID)
	return chain, chain.(*auditedProvider)
}

func findModel(t *testing.T, models []ModelInfo, id string) ModelInfo {
	t.Helper()
	for _, m := range models {
		if m.ID == id {
			return m
		}
	}
	t.Fatalf("listing %s has no %q", modelIDs(models), id)
	return ModelInfo{}
}

func overrideEntry(id, kind, deployment string, window, output int) AzureRegisteredModel {
	return AzureRegisteredModel{EntryID: id, Kind: kind, Deployment: deployment, ContextWindow: window, MaxOutputTokens: output}
}

// The override beats the catalogue (a deployment named after its model) and
// the served-model memo; without one, both still size the entry.
func TestAuditedRegistry_OverridesBeatCatalogueAndServedModel(t *testing.T) {
	ResetAzureFoundryCaches()
	t.Cleanup(ResetAzureFoundryCaches)
	p := registryTestProvider(t, nil, azureKeyConfig("https://my-res.openai.azure.com"), []AzureRegisteredModel{
		overrideEntry("aaaaaaaaa1", AzureModelKindChat, "gpt-4o", 32768, 2048),
		overrideEntry("aaaaaaaaa2", AzureModelKindChat, "custom", 65536, 4096),
		overrideEntry("aaaaaaaaa3", AzureModelKindChat, "plain", 0, 0),
		overrideEntry("aaaaaaaaa4", AzureModelKindChat, "gpt-4o-mini", 0, 1000),
	})
	chain, ap := registryChain(t, p)
	recordServedModel(ap.memoScope, azureFoundryProviderID, "custom", "gpt-4.1-2025-04-14")
	recordServedModel(ap.memoScope, azureFoundryProviderID, "plain", "gpt-4.1-2025-04-14")

	ctx := context.Background()
	for _, tc := range []struct {
		id             string
		window, output int
	}{
		{"gpt-4o", 32768, 2048},       // catalogue says 128000 / 16384
		{"custom", 65536, 4096},       // the memo says gpt-4.1: 1047576 / 32768
		{"plain", 1047576, 32768},     // no override: the memo sizes it
		{"gpt-4o-mini", 128000, 1000}, // the max output alone
	} {
		if got := ContextLimitFor(ctx, chain, tc.id); got != tc.window {
			t.Errorf("%s: ContextLimitFor = %d, want %d", tc.id, got, tc.window)
		}
		if got := ModelMaxOutputTokens(ctx, chain, tc.id); got != tc.output {
			t.Errorf("%s: ModelMaxOutputTokens = %d, want %d", tc.id, got, tc.output)
		}
	}
	models, _ := chain.Models(ctx)
	if m := findModel(t, models, "custom"); m.ServedModel != "gpt-4.1-2025-04-14" {
		t.Errorf("custom = %+v: the override resizes the entry, the memo still names its model", m)
	}
}

// A deployment's own context-length rejection is more authoritative than the
// user's guess: the learned window clamps the override, and the output cap
// with it.
func TestAuditedRegistry_LearnedWindowStillClamps(t *testing.T) {
	ResetAzureFoundryCaches()
	t.Cleanup(ResetAzureFoundryCaches)
	p := registryTestProvider(t, nil, azureKeyConfig("https://my-res.openai.azure.com"), []AzureRegisteredModel{
		overrideEntry("aaaaaaaaa1", AzureModelKindChat, "distilled", 65536, 8192),
		overrideEntry("aaaaaaaaa2", AzureModelKindChat, "roomy", 8192, 1024),
	})
	chain, ap := registryChain(t, p)
	recordContextWindow(ap.memoScope, azureFoundryProviderID, "distilled", 16384)
	recordContextWindow(ap.memoScope, azureFoundryProviderID, "roomy", 16384)
	ctx := context.Background()
	if got, out := ContextLimitFor(ctx, chain, "distilled"), ModelMaxOutputTokens(ctx, chain, "distilled"); got != 16384 || out != 4096 {
		t.Errorf("distilled = %d / %d, want the learned 16384 and a quarter of it as the output cap", got, out)
	}
	if got, out := ContextLimitFor(ctx, chain, "roomy"), ModelMaxOutputTokens(ctx, chain, "roomy"); got != 8192 || out != 1024 {
		t.Errorf("roomy = %d / %d, want the smaller override kept", got, out)
	}
}

// Every registered entry is listed whatever the live listing returned — cold
// (no project listing cached: every agent), a partial listing (the agents
// route failed), an agent the listing spells differently, a chat entry the
// listing re-lists as a Claude deployment — so ContextLimitFor and
// ModelMaxOutputTokens find it; the live data only decorates.
func TestAuditedRegistry_UnlistedEntriesAreListed(t *testing.T) {
	agentsStatus := 503
	srv := newAzureTestServer(t, func(w http.ResponseWriter, r *http.Request, _ int, _ map[string]any) {
		switch r.URL.Path {
		case "/api/projects/p1/deployments":
			writeJSON(w, 200, map[string]any{"value": []map[string]any{
				{"name": "prod-chat", "modelName": "gpt-4.1"},
				{"name": "c1", "modelName": "claude-sonnet-4-6", "modelPublisher": "Anthropic"},
			}})
		case "/api/projects/p1/agents":
			if agentsStatus != 200 {
				writeJSON(w, agentsStatus, map[string]any{"error": map[string]any{"message": "busy"}})
				return
			}
			writeJSON(w, 200, map[string]any{"data": []map[string]any{
				{"name": "helper", "definition": map[string]any{"kind": "prompt", "model": "prod-chat"}},
				{"name": "other", "definition": map[string]any{"kind": "prompt", "model": "prod-chat"}},
			}})
		default:
			t.Errorf("unexpected path %s", r.URL.Path)
			w.WriteHeader(404)
		}
	})
	ResetAzureFoundryCaches()
	t.Cleanup(ResetAzureFoundryCaches)
	entries := []AzureRegisteredModel{
		entry("aaaaaaaaa1", AzureModelKindChat, "prod-chat"),
		entry("aaaaaaaaa2", AzureModelKindChat, "c1"),
		overrideEntry("aaaaaaaaa3", AzureModelKindAgent, "Helper", 32000, 2048),
		entry("aaaaaaaaa4", AzureModelKindAgent, "Other"),
		entry("aaaaaaaaa5", AzureModelKindClaude, "sonnet"),
		entry("aaaaaaaaa6", AzureModelKindEmbedding, "kb-embed"),
	}
	build := func() Provider {
		chain, _ := registryChain(t, registryTestProvider(t, srv, registryEntraConfig(srv), entries))
		return chain
	}
	ctx := context.Background()

	// Cold: nothing cached, nothing fetched — every entry from the registry.
	cold := build()
	models, err := cold.Models(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n := len(srv.requests()); n != 0 {
		t.Fatalf("the chain's listing made %d requests, want none", n)
	}
	for _, id := range []string{"prod-chat", "c1", "agent:Helper", "agent:Other", "claude:sonnet", "kb-embed"} {
		findModel(t, models, id)
	}
	if got, out := ContextLimitFor(ctx, cold, "agent:Helper"), ModelMaxOutputTokens(ctx, cold, "agent:Helper"); got != 32000 || out != 2048 {
		t.Errorf("an unlisted agent's overrides = %d / %d, want 32000 / 2048", got, out)
	}
	if m := findModel(t, models, "claude:sonnet"); m.ContextLimit != claudeOnFoundryContextLimit || m.MaxOutputTokens != claudeOnFoundryDefaultOutput {
		t.Errorf("an unlisted Claude entry = %+v, want the Claude defaults", m)
	}
	if m := findModel(t, models, "kb-embed"); !m.Embedding {
		t.Errorf("an embedding entry = %+v, want it flagged", m)
	}

	// A partial listing (the agents route failed): the deployments decorate,
	// the agents still come from the registry.
	p := registryTestProvider(t, srv, registryEntraConfig(srv), entries)
	if err := p.refreshListing(ctx); err != nil {
		t.Fatal(err)
	}
	partial := build()
	models, _ = partial.Models(ctx)
	if m := findModel(t, models, "prod-chat"); m.ServedModel != "gpt-4.1" || m.ContextLimit != 1047576 {
		t.Errorf("prod-chat = %+v, want the project listing's model", m)
	}
	if got := ContextLimitFor(ctx, partial, "agent:Helper"); got != 32000 {
		t.Errorf("agent:Helper after a partial listing = %d, want its override", got)
	}
	// The listing re-lists c1 as a Claude deployment ("claude:c1", routed
	// there); the entry keeps its own id with the Claude deployment's window.
	if got := ContextLimitFor(ctx, partial, "c1"); got != 1000000 {
		t.Errorf("c1 re-listed as Claude = %d, want claude-sonnet-4-6's 1000000", got)
	}

	// A complete listing spelling the agents in lower case: the entries keep
	// the ids the chat request carries, with the listing's data — the
	// override still wins where there is one.
	agentsStatus = 200
	expireAzureListing(t, p.cfg)
	azureModelsCache.mu.Lock()
	delete(azureModelsCache.m, azureModelsCacheKey(p.cfg))
	azureModelsCache.mu.Unlock()
	if err := p.refreshListing(ctx); err != nil {
		t.Fatal(err)
	}
	listed := build()
	models, _ = listed.Models(ctx)
	if m := findModel(t, models, "agent:Other"); m.ServedModel != "gpt-4.1" || m.ContextLimit != 1047576 {
		t.Errorf("agent:Other = %+v, want the listed agent's model under the entry's id", m)
	}
	if m := findModel(t, models, "agent:Helper"); m.ServedModel != "gpt-4.1" || m.ContextLimit != 32000 || m.MaxOutputTokens != 2048 {
		t.Errorf("agent:Helper = %+v, want the listing's model with the override's window", m)
	}
}

// A provider wrapped directly (NewAuditedProvider) applies its registry too;
// a document that does not validate lists nothing (the chat fails on it
// anyway), so the provider-wide window applies.
func TestAuditedRegistry_DirectWrapAndInvalidDocument(t *testing.T) {
	ResetAzureFoundryCaches()
	t.Cleanup(ResetAzureFoundryCaches)
	entries := []AzureRegisteredModel{overrideEntry("aaaaaaaaa1", AzureModelKindChat, "gpt-4o", 32768, 2048)}
	p := registryTestProvider(t, nil, azureKeyConfig("https://my-res.openai.azure.com"), entries)
	direct := NewAuditedProvider(p, nil, "u1", azureFoundryProviderID)
	if got := ContextLimitFor(context.Background(), direct, "gpt-4o"); got != 32768 {
		t.Errorf("directly wrapped: %d, want the override", got)
	}

	bad := NewAzureFoundryConnectionProvider(`{"endpoint":"https://evil.example.com","auth":{"mode":"api_key","key":"k"}}`, entries, "")
	chain := (&ProviderFactory{}).wrapChain(bad, "u1", azureFoundryProviderID)
	if _, err := chain.Models(context.Background()); !errors.Is(err, ErrProviderConfigInvalid) {
		t.Errorf("invalid document listing: %v, want ErrProviderConfigInvalid", err)
	}
	if got := ContextLimitFor(context.Background(), chain, "gpt-4o"); got != 128000 {
		t.Errorf("invalid document window = %d, want the provider-wide default", got)
	}
}
