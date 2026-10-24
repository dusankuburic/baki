package ai

import "testing"

// TestAzureCatalog_UnpricedButKeepsLimits: Azure AI Foundry is unpriced (cost
// is read in the Azure portal), so every catalogue entry — current pro tiers
// and dated served names included — resolves to its own window/output cap
// (Microsoft's "models sold directly by Azure" figures) with a zero Pricing,
// and an unknown deployment does not resolve at all (the provider-wide
// window applies).
func TestAzureCatalog_UnpricedButKeepsLimits(t *testing.T) {
	for _, tc := range []struct {
		served         string
		window, output int
	}{
		{"gpt-5-pro-2025-10-06", 272000, 128000},
		{"o3-pro-2025-06-10", 200000, 100000},
		{"o1", 200000, 100000},
		{"MAI-Thinking-1", 256000, 64000},
		{"grok-4.3", 200000, 8192},
		{"grok-4.6", 72000, 128000},
		{"gpt-5.4-pro", 272000, 128000},
		{"DeepSeek-V4-Flash-0731", 1000000, 384000},
		{"gpt-5.5-2026-04-24", 272000, 128000},
	} {
		entry, ok := catalogEntryFor(azureFoundryProviderID, tc.served)
		if !ok {
			t.Errorf("%s does not resolve — its window/output cap would fall back to the provider-wide default", tc.served)
			continue
		}
		if entry.ContextLimit != tc.window || entry.MaxOutputTokens != tc.output {
			t.Errorf("%s limits = %d / %d, want %d / %d", tc.served, entry.ContextLimit, entry.MaxOutputTokens, tc.window, tc.output)
		}
	}
	if e, ok := catalogEntryFor(azureFoundryProviderID, "definitely-not-a-model"); ok || e != (ModelInfo{}) {
		t.Errorf("unknown deployment resolved to %+v (%v), want no entry", e, ok)
	}
	// catalogEntryFor is the single guard: the Claude rows an Azure Claude
	// deployment borrows come back unpriced there, and keep their prices for
	// the direct Claude provider.
	if e, ok := catalogEntryFor(azureFoundryProviderID, "claude:claude-opus-5"); !ok || e.ContextLimit != 1000000 {
		t.Errorf("Azure Claude entry = %+v (%v), want the 1M window", e, ok)
	}
	if e, ok := catalogEntryFor("claude", "claude-opus-5"); !ok {
		t.Errorf("direct Claude entry = %+v (%v), want the catalogue row", e, ok)
	}
}

// TestAzureCatalog_Grok4LimitsPinnedToMicrosoftDocs: grok-4's documented
// limits are 262,000 input tokens and 8,192 output tokens. The service
// re-caps at the listing's MaxOutputTokens (normalizeChatParams), so the
// catalogue number is what protects the turn.
func TestAzureCatalog_Grok4LimitsPinnedToMicrosoftDocs(t *testing.T) {
	entry, ok := catalogEntryFor(azureFoundryProviderID, "grok-4")
	if !ok || entry.ContextLimit != 262000 || entry.MaxOutputTokens != 8192 {
		t.Errorf("grok-4 entry = %+v %v, want 262000 context / 8192 output", entry, ok)
	}
	// The reasoning floor exceeds the cap on purpose: ReasoningOutputFloor
	// raises a 4096 default to 16384 and the chat service re-clamps it to the
	// model's cap, so an 8192 cap means grok-4 turns ask for their true max.
	if floor := ReasoningOutputFloor("grok-4"); floor <= entry.MaxOutputTokens {
		t.Errorf("grok-4 floor %d must exceed its cap %d only through the service's re-clamp", floor, entry.MaxOutputTokens)
	}
}
