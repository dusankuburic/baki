package ai

import (
	"context"
	"fmt"
	"slices"
	"testing"

	"pad-core/cache"
)

// The provider memos (R-provider-31) are declared caches: least-recently-used eviction at their cap, never a wholesale
// reset nor a random delete. Each row fills its memo one past the cap; only the oldest entry may go.
func TestProviderMemos_EvictOnlyTheOldestAtTheirCap(t *testing.T) {
	const az = azureFoundryProviderID
	key := func(i int) string { return fmt.Sprintf("https://r.example/v1\x00dep-%d", i) }
	dims := func(i int) *openaiBase {
		u := "https://r.example/v1/embeddings"
		return &openaiBase{baseURL: &u, embeddingModel: fmt.Sprintf("emb-%d", i)}
	}
	scope := func(i int) string { return fmt.Sprintf("user-%d\x00https://r.example", i) }
	for _, m := range []struct {
		name  string
		max   int
		reset func()
		put   func(i int)
		has   func(i int) bool
	}{
		{"learnedRepairs", learnedRepairsMax, resetLearnedRepairs,
			func(i int) { rememberRepairs(key(i), map[string]bool{"temperature": true}) },
			func(i int) bool { return len(repairsFor(key(i))) > 0 }},
		{"streamRefusals", learnedRepairsMax, resetLearnedRepairs,
			func(i int) { rememberStreamRefusal(key(i)) },
			func(i int) bool { return streamRefused(key(i)) }},
		{"responsesToolRoutes", learnedRepairsMax, resetResponsesToolRoutes,
			func(i int) { rememberResponsesRoute(key(i), true) },
			func(i int) bool { _, known := responsesRouteFor(key(i)); return known }},
		{"agentKinds", servedMemoMax, resetAgentKinds,
			func(i int) {
				rememberAgentKind("https://r.example/api/projects/p", fmt.Sprintf("agent-%d", i), agentKindWorkflow)
			},
			func(i int) bool {
				return agentKindFor("https://r.example/api/projects/p", fmt.Sprintf("agent-%d", i)) != ""
			}},
		{"servedMemo", servedMemoMax, resetServedModels,
			func(i int) { recordServedModel(scope(i), az, "chat", "gpt-4.1-2025-04-14") },
			func(i int) bool { _, ok := lookupServedModel(scope(i), az, "chat"); return ok }},
		{"learnedWindows", servedMemoMax, resetLearnedWindows,
			func(i int) { recordContextWindow(scope(i), az, "chat", 16384) },
			func(i int) bool { _, ok := lookupContextWindow(scope(i), az, "chat"); return ok }},
		{"embedDimsRefusals", servedMemoMax, resetEmbedDimsRefusals,
			func(i int) { dims(i).rememberDimensionsRefusal() },
			func(i int) bool { return dims(i).dimensionsRefused() }},
	} {
		t.Run(m.name, func(t *testing.T) {
			m.reset()
			t.Cleanup(m.reset)
			for i := 0; i <= m.max; i++ {
				m.put(i)
			}
			if m.has(0) {
				t.Errorf("the oldest of %d entries survived a memo capped at %d", m.max+1, m.max)
			}
			lost := 0
			for i := 1; i <= m.max; i++ {
				if !m.has(i) {
					lost++
				}
			}
			if lost > 0 {
				t.Errorf("%d of the %d newest entries were dropped; only the oldest may go", lost, m.max)
			}
		})
	}
}

// Every provider memo is a declared, replica-local cache (the caches golden lists them).
func TestProviderMemos_AreDeclaredReplicaLocalCaches(t *testing.T) {
	want := []string{"ai.agent-kinds", "ai.context-windows", "ai.embed-dims-refusals", "ai.param-repairs",
		"ai.responses-tool-routes", "ai.served-by-resource", "ai.served-models", "ai.stream-refusals"}
	got := map[string]cache.Locality{}
	for _, s := range cache.Specs() {
		got[s.Name] = s.Locality
	}
	for _, name := range want {
		loc, ok := got[name]
		if !ok {
			t.Errorf("cache %q is not declared", name)
			continue
		}
		if loc != cache.LocalityReplica {
			t.Errorf("cache %q is %s, want replica-local", name, loc)
		}
	}
}

// rememberRepairs merges (R-provider-31): a deployment that learned temperature keeps it after a later request to the
// same key, which did not send temperature, learns max_tokens.
func TestRememberRepairs_ALearnedTemperatureSurvivesALaterMaxTokensRepair(t *testing.T) {
	resetLearnedRepairs()
	t.Cleanup(resetLearnedRepairs)
	srv := newRepairTestServer(t)
	rejectMaxTokens := false
	srv.setReject(func(body map[string]any) (int, map[string]any) {
		if _, has := body["temperature"]; has {
			return rejectParam("temperature")
		}
		if _, has := body[maxTokensParamLegacy]; has && rejectMaxTokens {
			return rejectParam(maxTokensParamLegacy)
		}
		return 0, nil
	})
	p := compatErrProvider(srv.Client(), srv.URL+"/chat/completions")
	msgs := []Message{{Role: "user", Content: "hi"}}

	if _, err := p.Chat(context.Background(), Request{Model: "o4-mini", Temperature: 0.3, Messages: msgs}); err != nil {
		t.Fatalf("turn 1: %v", err)
	}
	srv.setReject(func(body map[string]any) (int, map[string]any) {
		if _, has := body["temperature"]; has {
			return rejectParam("temperature")
		}
		if _, has := body[maxTokensParamLegacy]; has {
			return rejectParam(maxTokensParamLegacy)
		}
		return 0, nil
	})
	// Turn 2 sends no temperature and learns the max_tokens swap.
	if _, err := p.Chat(context.Background(), Request{Model: "o4-mini", MaxTokens: 64, Messages: msgs}); err != nil {
		t.Fatalf("turn 2: %v", err)
	}
	got := repairsFor(p.repairsKey("o4-mini"))
	if !slices.Contains(got, "temperature") || !slices.Contains(got, maxTokensParamLegacy) {
		t.Fatalf("memo = %v, want temperature and %s", got, maxTokensParamLegacy)
	}
	// Turn 3 sends both and pays no rejection.
	before := srv.count()
	if _, err := p.Chat(context.Background(), Request{Model: "o4-mini", Temperature: 0.3, MaxTokens: 64, Messages: msgs}); err != nil {
		t.Fatalf("turn 3: %v", err)
	}
	if n := srv.count() - before; n != 1 {
		t.Errorf("turn 3 cost %d requests, want 1 (both repairs pre-applied)", n)
	}
}

// A request without tools that learns another repair keeps the learned reasoning_effort=none (applied, and so recorded
// again, only on requests with tools).
func TestRememberRepairs_KeepsTheToolsReasoningRepairAcrossAToolLessRepair(t *testing.T) {
	resetLearnedRepairs()
	t.Cleanup(resetLearnedRepairs)
	key := "https://r.example/v1/chat/completions\x00chat"
	rememberRepairs(key, map[string]bool{reasoningEffortNoneRepair: true})
	rememberRepairs(key, map[string]bool{"temperature": true})
	if got := repairsFor(key); !slices.Equal(got, []string{reasoningEffortNoneRepair, "temperature"}) {
		t.Errorf("memo = %v, want [%s temperature]", got, reasoningEffortNoneRepair)
	}
}

// The resource-wide view is keyed by the same resource the lookup asks with: with a project endpoint the memo scope
// is owner, endpoint and project, and claudeServedModel asks with endpoint and project.
func TestServedModelForResource_ResolvesWithAProjectEndpoint(t *testing.T) {
	resetServedModels()
	t.Cleanup(resetServedModels)
	p := &AzureFoundryProvider{cfg: AzureFoundryConfig{Endpoint: "https://res.services.ai.azure.com",
		ProjectEndpoint: "https://res.services.ai.azure.com/api/projects/p1"}}
	res := p.servedMemoResource()
	recordServedModel("u1\x00"+res, azureFoundryProviderID, "prod-claude", "claude-opus-4-6")
	if got, ok := lookupServedModelForResource(res, azureFoundryProviderID, "prod-claude"); !ok || got != "claude-opus-4-6" {
		t.Errorf("resource lookup = %q, %v; want claude-opus-4-6, true", got, ok)
	}
	// Another project of the same resource is another resource.
	other := (&AzureFoundryProvider{cfg: AzureFoundryConfig{Endpoint: "https://res.services.ai.azure.com",
		ProjectEndpoint: "https://res.services.ai.azure.com/api/projects/p2"}}).servedMemoResource()
	if got, ok := lookupServedModelForResource(other, azureFoundryProviderID, "prod-claude"); ok {
		t.Errorf("another project's lookup = %q", got)
	}
}

// One user's varied observation (a deployment re-pointed under them) does not wipe another user's stable one: the
// view answers with the most recent stable observation, as the old per-user index did.
func TestServedModelForResource_AVariedUserDoesNotWipeAnotherUsersStableObservation(t *testing.T) {
	resetServedModels()
	t.Cleanup(resetServedModels)
	const az, res = azureFoundryProviderID, "https://res.services.ai.azure.com"
	recordServedModel("userA\x00"+res, az, "chat", "gpt-4.1-2025-04-14")
	recordServedModel("userA\x00"+res, az, "chat", "gpt-5-2025-08-07") // re-pointed: A's entry is varied
	recordServedModel("userB\x00"+res, az, "chat", "gpt-5-2025-08-07") // B sees the stable model
	if got, ok := lookupServedModelForResource(res, az, "chat"); !ok || got != "gpt-5-2025-08-07" {
		t.Fatalf("after B's stable observation: %q, %v; want gpt-5-2025-08-07, true", got, ok)
	}
	recordServedModel("userA\x00"+res, az, "chat", "gpt-5-2025-08-07") // A's entry stays varied within the TTL
	if got, ok := lookupServedModelForResource(res, az, "chat"); !ok || got != "gpt-5-2025-08-07" {
		t.Errorf("after A's next call: %q, %v; want B's stable observation still served", got, ok)
	}
}
