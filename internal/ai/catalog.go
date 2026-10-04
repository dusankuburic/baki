package ai

import (
	"regexp"
	"strings"
)

// modelCatalog is the single source of truth for the models each provider serves, with pricing and limits —
// the data that drifts as vendors ship models and change prices, centralized in one auditable place.
// TestProviderCatalogConsistency validates each provider's DefaultModel/FreeModel against these entries.
var modelCatalog = map[string][]ModelInfo{
	// Claude: prices and limits as published on platform.claude.com, checked 2026-09-28. The prices bill the
	// direct Claude provider only; a Foundry Claude deployment borrows these rows for its window/cap, unpriced.
	"claude": {
		{ID: "claude-fable-5-1", DisplayName: "Claude Fable 5.1", ContextLimit: 1000000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 10.0, OutputCostPerM: 50.0}},
		{ID: "claude-fable-5", DisplayName: "Claude Fable 5", ContextLimit: 1000000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 10.0, OutputCostPerM: 50.0}},
		{ID: "claude-mythos-5-1", DisplayName: "Claude Mythos 5.1", ContextLimit: 1000000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 10.0, OutputCostPerM: 50.0}},
		{ID: "claude-mythos-5", DisplayName: "Claude Mythos 5", ContextLimit: 1000000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 10.0, OutputCostPerM: 50.0}},
		// claude-mythos-preview: Foundry lists it but Anthropic publishes no
		// price row of its own — the Mythos 5/5.1 rate is assumed.
		{ID: "claude-mythos-preview", DisplayName: "Claude Mythos Preview", ContextLimit: 1000000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 10.0, OutputCostPerM: 50.0}},
		{ID: "claude-opus-5-5", DisplayName: "Claude Opus 5.5", ContextLimit: 1000000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 4.0, OutputCostPerM: 20.0}},
		{ID: "claude-opus-5", DisplayName: "Claude Opus 5", ContextLimit: 1000000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 5.0, OutputCostPerM: 25.0}},
		{ID: "claude-opus-4-8", DisplayName: "Claude Opus 4.8", ContextLimit: 1000000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 5.0, OutputCostPerM: 25.0}},
		{ID: "claude-opus-4-7", DisplayName: "Claude Opus 4.7", ContextLimit: 1000000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 5.0, OutputCostPerM: 25.0}},
		{ID: "claude-opus-4-6", DisplayName: "Claude Opus 4.6", ContextLimit: 1000000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 5.0, OutputCostPerM: 25.0}},
		{ID: "claude-opus-4-5", DisplayName: "Claude Opus 4.5", ContextLimit: 200000, MaxOutputTokens: 64000, Pricing: Pricing{InputCostPerM: 5.0, OutputCostPerM: 25.0}},
		{ID: "claude-sonnet-5-5", DisplayName: "Claude Sonnet 5.5", ContextLimit: 1000000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 2.0, OutputCostPerM: 10.0}},
		{ID: "claude-sonnet-5", DisplayName: "Claude Sonnet 5", ContextLimit: 1000000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 2.0, OutputCostPerM: 10.0}},
		{ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6", ContextLimit: 1000000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 3.0, OutputCostPerM: 15.0}},
		{ID: "claude-sonnet-4-5", DisplayName: "Claude Sonnet 4.5", ContextLimit: 200000, MaxOutputTokens: 64000, Pricing: Pricing{InputCostPerM: 3.0, OutputCostPerM: 15.0}},
		{ID: "claude-haiku-4-5", DisplayName: "Claude Haiku 4.5", ContextLimit: 200000, MaxOutputTokens: 64000, Pricing: Pricing{InputCostPerM: 1.0, OutputCostPerM: 5.0}},
	},
	"openai": {
		{ID: "gpt-4o", DisplayName: "GPT-4o", ContextLimit: 128000, Pricing: Pricing{InputCostPerM: 2.5, OutputCostPerM: 10.0}},
		{ID: "gpt-4o-mini", DisplayName: "GPT-4o Mini", ContextLimit: 128000, Pricing: Pricing{InputCostPerM: 0.15, OutputCostPerM: 0.6}},
		{ID: "gpt-4-turbo", DisplayName: "GPT-4 Turbo", ContextLimit: 128000, Pricing: Pricing{InputCostPerM: 10.0, OutputCostPerM: 30.0}},
	},
	"gemini": {
		{ID: "gemini-2.5-pro", DisplayName: "Gemini 2.5 Pro", ContextLimit: 1048576, Pricing: Pricing{InputCostPerM: 1.25, OutputCostPerM: 10.0}},
		{ID: "gemini-2.5-flash", DisplayName: "Gemini 2.5 Flash", ContextLimit: 1048576, Pricing: Pricing{InputCostPerM: 0.15, OutputCostPerM: 0.6}},
	},
	"xai": {
		{ID: "grok-3", DisplayName: "Grok 3", ContextLimit: 131072, Pricing: Pricing{InputCostPerM: 3.0, OutputCostPerM: 15.0}},
		{ID: "grok-3-mini", DisplayName: "Grok 3 Mini", ContextLimit: 131072, Pricing: Pricing{InputCostPerM: 0.3, OutputCostPerM: 0.5}},
		{ID: "grok-2", DisplayName: "Grok 2", ContextLimit: 131072, Pricing: Pricing{InputCostPerM: 2.0, OutputCostPerM: 10.0}},
	},
	"glm": {
		// glm-5.3 and glm-5.2 verified against the z.ai coding endpoint 2026-09-23; the -turbo/-air/-flash
		// variants of either do not exist there (1211 Unknown Model).
		{ID: "glm-5.3", DisplayName: "GLM-5.3", ContextLimit: 200000, Pricing: Pricing{InputCostPerM: 1.4, OutputCostPerM: 4.4}},
		{ID: "glm-5.2", DisplayName: "GLM-5.2", ContextLimit: 200000, Pricing: Pricing{InputCostPerM: 1.4, OutputCostPerM: 4.4}},
		{ID: "glm-5.1", DisplayName: "GLM-5.1", ContextLimit: 200000, Pricing: Pricing{InputCostPerM: 1.4, OutputCostPerM: 4.4}},
		{ID: "glm-5", DisplayName: "GLM-5", ContextLimit: 200000, Pricing: Pricing{InputCostPerM: 1.0, OutputCostPerM: 3.2}},
		{ID: "glm-5-turbo", DisplayName: "GLM-5 Turbo", ContextLimit: 200000, Pricing: Pricing{InputCostPerM: 0.8, OutputCostPerM: 2.4}},
	},
	// GitHub Models' free tier enforces an 8 192-token request limit regardless of native window; the
	// limits cap the context budget so the API never returns a 413.
	"github-models": {
		{ID: "gpt-4o", DisplayName: "GPT-4o", ContextLimit: 8192, Pricing: Pricing{InputCostPerM: 2.5, OutputCostPerM: 10.0}},
		{ID: "gpt-4o-mini", DisplayName: "GPT-4o Mini", ContextLimit: 8192, Pricing: Pricing{InputCostPerM: 0.15, OutputCostPerM: 0.6}},
		{ID: "Meta-Llama-3.3-70B-Instruct", DisplayName: "Llama 3.3 70B", ContextLimit: 8192, Pricing: Pricing{InputCostPerM: 0.7, OutputCostPerM: 0.9}},
		{ID: "Mistral-large-2411", DisplayName: "Mistral Large", ContextLimit: 8192, Pricing: Pricing{InputCostPerM: 2.0, OutputCostPerM: 6.0}},
		{ID: "Phi-4", DisplayName: "Phi-4", ContextLimit: 8192, Pricing: Pricing{InputCostPerM: 0.07, OutputCostPerM: 0.14}},
		{ID: "DeepSeek-V3-0324", DisplayName: "DeepSeek V3", ContextLimit: 8192, Pricing: Pricing{InputCostPerM: 0.49, OutputCostPerM: 0.94}},
		{ID: "ai21-jamba-1.5-large", DisplayName: "Jamba 1.5 Large", ContextLimit: 8192, Pricing: Pricing{InputCostPerM: 2.0, OutputCostPerM: 8.0}},
	},
	"copilot": {
		{ID: "gpt-4o", DisplayName: "GPT-4o", ContextLimit: 128000, Pricing: Pricing{InputCostPerM: 0, OutputCostPerM: 0}},
		{ID: "gpt-4o-mini", DisplayName: "GPT-4o Mini", ContextLimit: 128000, Pricing: Pricing{InputCostPerM: 0, OutputCostPerM: 0}},
		{ID: "claude-3.5-sonnet", DisplayName: "Claude 3.5 Sonnet", ContextLimit: 200000, Pricing: Pricing{InputCostPerM: 0, OutputCostPerM: 0}},
	},
	// Azure AI Foundry serves whatever the user DEPLOYED under a chosen name, so this list is the reference
	// table the served-model name is sized against. ContextLimit is what the prompt is truncated against:
	// where Azure documents separate input and output limits (gpt-5 family: 272k in + 128k out) the INPUT
	// figure goes here, never the total window. No pricing — the Azure portal is the source of truth. Windows
	// from Microsoft's "models sold directly by Azure" page, checked 2026-09-29; gpt-5.4+ and gpt-6 accept up
	// to 922,000 input tokens but bill beyond 272,000 at a higher long-context tier, so 272,000 it is.
	"azure-foundry": {
		{ID: "gpt-4.1", DisplayName: "GPT-4.1", ContextLimit: 1047576, MaxOutputTokens: 32768},
		{ID: "gpt-4.1-mini", DisplayName: "GPT-4.1 Mini", ContextLimit: 1047576, MaxOutputTokens: 32768},
		{ID: "gpt-4.1-nano", DisplayName: "GPT-4.1 Nano", ContextLimit: 1047576, MaxOutputTokens: 32768},
		{ID: "gpt-4o", DisplayName: "GPT-4o", ContextLimit: 128000, MaxOutputTokens: 16384},
		{ID: "gpt-4o-mini", DisplayName: "GPT-4o Mini", ContextLimit: 128000, MaxOutputTokens: 16384},
		{ID: "gpt-5", DisplayName: "GPT-5", ContextLimit: 272000, MaxOutputTokens: 128000},
		{ID: "gpt-5-mini", DisplayName: "GPT-5 Mini", ContextLimit: 272000, MaxOutputTokens: 128000},
		{ID: "gpt-5-nano", DisplayName: "GPT-5 Nano", ContextLimit: 272000, MaxOutputTokens: 128000},
		// gpt-5-chat is retired (June 2026) but kept: legacy deployments still
		// serving it must stay sized.
		{ID: "gpt-5-chat", DisplayName: "GPT-5 Chat", ContextLimit: 128000, MaxOutputTokens: 16384},
		{ID: "gpt-5-codex", DisplayName: "GPT-5 Codex", ContextLimit: 272000, MaxOutputTokens: 128000},
		{ID: "gpt-5-pro", DisplayName: "GPT-5 Pro", ContextLimit: 272000, MaxOutputTokens: 128000},
		{ID: "gpt-5.1", DisplayName: "GPT-5.1", ContextLimit: 272000, MaxOutputTokens: 128000},
		{ID: "gpt-5.1-codex", DisplayName: "GPT-5.1 Codex", ContextLimit: 272000, MaxOutputTokens: 128000},
		{ID: "gpt-5.1-codex-mini", DisplayName: "GPT-5.1 Codex Mini", ContextLimit: 272000, MaxOutputTokens: 128000},
		{ID: "gpt-5.1-codex-max", DisplayName: "GPT-5.1 Codex Max", ContextLimit: 272000, MaxOutputTokens: 128000},
		{ID: "gpt-5.2", DisplayName: "GPT-5.2", ContextLimit: 272000, MaxOutputTokens: 128000},
		{ID: "gpt-5.2-codex", DisplayName: "GPT-5.2 Codex", ContextLimit: 272000, MaxOutputTokens: 128000},
		{ID: "gpt-5.3-codex", DisplayName: "GPT-5.3 Codex", ContextLimit: 272000, MaxOutputTokens: 128000},
		{ID: "gpt-5.4", DisplayName: "GPT-5.4", ContextLimit: 272000, MaxOutputTokens: 128000},
		{ID: "gpt-5.4-mini", DisplayName: "GPT-5.4 Mini", ContextLimit: 272000, MaxOutputTokens: 128000},
		{ID: "gpt-5.4-nano", DisplayName: "GPT-5.4 Nano", ContextLimit: 272000, MaxOutputTokens: 128000},
		{ID: "gpt-5.4-pro", DisplayName: "GPT-5.4 Pro", ContextLimit: 272000, MaxOutputTokens: 128000},
		{ID: "gpt-5.5", DisplayName: "GPT-5.5", ContextLimit: 272000, MaxOutputTokens: 128000},
		{ID: "gpt-5.6-sol", DisplayName: "GPT-5.6 Sol", ContextLimit: 272000, MaxOutputTokens: 128000},
		{ID: "gpt-5.6-terra", DisplayName: "GPT-5.6 Terra", ContextLimit: 272000, MaxOutputTokens: 128000},
		{ID: "gpt-5.6-luna", DisplayName: "GPT-5.6 Luna", ContextLimit: 272000, MaxOutputTokens: 128000},
		{ID: "gpt-6-astra", DisplayName: "GPT-6 Astra", ContextLimit: 272000, MaxOutputTokens: 128000},
		{ID: "gpt-6-sol", DisplayName: "GPT-6 Sol", ContextLimit: 272000, MaxOutputTokens: 128000},
		{ID: "gpt-6-luna", DisplayName: "GPT-6 Luna", ContextLimit: 272000, MaxOutputTokens: 128000},
		// gpt-chat-latest is a moving alias for the dated chat model it
		// currently points at (2026-08-06).
		{ID: "gpt-chat-latest", DisplayName: "GPT chat-latest", ContextLimit: 272000, MaxOutputTokens: 128000},
		{ID: "o3", DisplayName: "o3", ContextLimit: 200000, MaxOutputTokens: 100000},
		{ID: "o3-mini", DisplayName: "o3-mini", ContextLimit: 200000, MaxOutputTokens: 100000},
		{ID: "o3-pro", DisplayName: "o3 Pro", ContextLimit: 200000, MaxOutputTokens: 100000},
		// o3-deep-research is sold via Foundry Agent Service only; Microsoft
		// publishes no output cap, so none is enforced (0 = unknown).
		{ID: "o3-deep-research", DisplayName: "o3 Deep Research", ContextLimit: 200000},
		{ID: "o4-mini", DisplayName: "o4-mini", ContextLimit: 200000, MaxOutputTokens: 100000},
		{ID: "o1", DisplayName: "o1", ContextLimit: 200000, MaxOutputTokens: 100000},
		{ID: "o1-mini", DisplayName: "o1-mini", ContextLimit: 128000, MaxOutputTokens: 65536},
		{ID: "codex-mini", DisplayName: "codex mini", ContextLimit: 200000, MaxOutputTokens: 100000},
		{ID: "gpt-oss-120b", DisplayName: "gpt-oss 120B", ContextLimit: 131072, MaxOutputTokens: 32768},
		// gpt-oss-20b is served via managed compute / Foundry Local only.
		{ID: "gpt-oss-20b", DisplayName: "gpt-oss 20B", ContextLimit: 131072, MaxOutputTokens: 131072},
		// model-router: the routed-to model is what the response reports; the
		// entry keeps the conservative 128K window.
		{ID: "model-router", DisplayName: "Model router", ContextLimit: 128000},
		{ID: "MAI-Thinking-1", DisplayName: "MAI-Thinking-1", ContextLimit: 256000, MaxOutputTokens: 64000},
		// DeepSeek-V3-0324/V3.1/R1-0528 are retired but kept for legacy
		// deployments. The -0731 preview's dated ID gets its own entry.
		{ID: "DeepSeek-V3-0324", DisplayName: "DeepSeek V3", ContextLimit: 131072, MaxOutputTokens: 32768},
		{ID: "DeepSeek-V3.1", DisplayName: "DeepSeek V3.1", ContextLimit: 131072, MaxOutputTokens: 32768},
		{ID: "DeepSeek-R1-0528", DisplayName: "DeepSeek R1", ContextLimit: 131072, MaxOutputTokens: 32768},
		{ID: "DeepSeek-V3.2", DisplayName: "DeepSeek V3.2", ContextLimit: 128000, MaxOutputTokens: 128000},
		{ID: "DeepSeek-V3.2-Speciale", DisplayName: "DeepSeek V3.2 Speciale", ContextLimit: 128000, MaxOutputTokens: 128000},
		{ID: "DeepSeek-V4-Flash", DisplayName: "DeepSeek V4 Flash", ContextLimit: 1000000, MaxOutputTokens: 384000},
		{ID: "DeepSeek-V4-Flash-0731", DisplayName: "DeepSeek V4 Flash 0731", ContextLimit: 1000000, MaxOutputTokens: 384000},
		{ID: "DeepSeek-V4-Pro", DisplayName: "DeepSeek V4 Pro", ContextLimit: 1000000, MaxOutputTokens: 384000},
		{ID: "Llama-4-Maverick-17B-128E-Instruct-FP8", DisplayName: "Llama 4 Maverick", ContextLimit: 1000000, MaxOutputTokens: 32768},
		// grok-3/-3-mini are retired but kept for legacy deployments.
		{ID: "grok-3", DisplayName: "Grok 3", ContextLimit: 131072, MaxOutputTokens: 32768},
		{ID: "grok-3-mini", DisplayName: "Grok 3 Mini", ContextLimit: 131072, MaxOutputTokens: 32768},
		// Grok windows/outputs per Microsoft's model list; grok-4.6's 200k window is shared with its 128k
		// output, so the entry stores the 72k input share.
		{ID: "grok-4", DisplayName: "Grok 4", ContextLimit: 262000, MaxOutputTokens: 8192},
		{ID: "grok-4.1-fast-reasoning", DisplayName: "Grok 4.1 Fast Reasoning", ContextLimit: 128000, MaxOutputTokens: 128000},
		{ID: "grok-4.1-fast-non-reasoning", DisplayName: "Grok 4.1 Fast", ContextLimit: 128000, MaxOutputTokens: 128000},
		{ID: "grok-4.3", DisplayName: "Grok 4.3", ContextLimit: 200000, MaxOutputTokens: 8192},
		{ID: "grok-4.6", DisplayName: "Grok 4.6", ContextLimit: 72000, MaxOutputTokens: 128000},
		{ID: "grok-code-fast-1", DisplayName: "Grok Code Fast 1", ContextLimit: 256000, MaxOutputTokens: 8192},
		{ID: "Phi-4", DisplayName: "Phi-4", ContextLimit: 16384, MaxOutputTokens: 8192},
		{ID: "text-embedding-3-small", DisplayName: "text-embedding-3-small", ContextLimit: 8191},
		{ID: "text-embedding-3-large", DisplayName: "text-embedding-3-large", ContextLimit: 8191},
		{ID: "text-embedding-ada-002", DisplayName: "text-embedding-ada-002", ContextLimit: 8191},
	},
}

// cacheReadMultiplier is the fraction of the base input rate a cached-input token is billed at for the model
// a call named or was served by. Anthropic publishes per-family rates (0.025x on Fable/Mythos 5.1, 0.05x on
// Opus 5.5, 0.1x on every other Claude); everyone else keeps the full input rate.
func cacheReadMultiplier(providerID, modelID, servedModel string) float64 {
	claudeRoute := providerID == "claude" || IsClaudeModel(modelID) || isClaudeModelName(servedModel)
	if !claudeRoute {
		return 1.0
	}
	for _, n := range []string{servedModel, modelID} {
		n = strings.ToLower(strings.TrimSpace(n))
		if n == "" {
			continue
		}
		switch {
		case strings.Contains(n, "fable-5-1"), strings.Contains(n, "mythos-5-1"):
			return 0.025
		case strings.Contains(n, "opus-5-5"):
			return 0.05
		}
	}
	return 0.1
}

// cacheWriteMultiplier is the rate a 5-minute prompt-cache WRITE token is
// billed at relative to the base input rate (Anthropic: 1.25x; would be 2x
// for a 1h TTL, which baki never sends).
const cacheWriteMultiplier = 1.25

// claudeInviteOnly lists catalogued Claude models offered by invitation only (Project Glasswing): kept so a
// Foundry deployment of one is sized, but not offered in the direct provider's picker.
var claudeInviteOnly = map[string]bool{
	"claude-mythos-5-1":     true,
	"claude-mythos-5":       true,
	"claude-mythos-preview": true,
}

// isClaudeModelName reports whether a model name (a served model, a project
// listing's modelName) is a Claude model.
func isClaudeModelName(name string) bool {
	return strings.HasPrefix(normalizeServedModel(name), "claude")
}

// servedModelDateSuffix strips the version date OpenAI-compatible responses
// append to the model name ("gpt-4o-2024-11-20" → "gpt-4o").
var servedModelDateSuffix = regexp.MustCompile(`-\d{4}-\d{2}-\d{2}$`)

// normalizeServedModel canonicalises a response "model" value for catalogue
// lookup: lower-cased, trimmed, version date removed.
func normalizeServedModel(served string) string {
	s := strings.ToLower(strings.TrimSpace(served))
	return servedModelDateSuffix.ReplaceAllString(s, "")
}

// catalogEntryFor finds the catalogue entry for the model a provider reported serving: exact on the
// normalised name, or — for names that only add a numeric/date version to a catalogued ID ("gpt-4.1-2025-04-14")
// — the longest catalogued ID the name extends. A sibling model ("gpt-5-mini" when only "gpt-5" is catalogued)
// is NOT matched: silent mispricing would be worse than the explicit fallback.
// Foundry's Claude deployments are sized by the Claude catalogue, and every Azure entry comes back unpriced.
func catalogEntryFor(providerID, served string) (ModelInfo, bool) {
	e, ok := catalogEntryLookup(providerID, served)
	if ok && providerID == azureFoundryProviderID {
		// Azure AI Foundry is deliberately unpriced: the catalogue supplies
		// windows and output limits only; the cost is read in the portal.
		e.Pricing = Pricing{}
	}
	return e, ok
}

func catalogEntryLookup(providerID, served string) (ModelInfo, bool) {
	if providerID == azureFoundryProviderID && IsClaudeModel(served) {
		return catalogEntryLookup("claude", claudeDeploymentName(served))
	}
	norm := normalizeServedModel(served)
	if norm == "" {
		return ModelInfo{}, false
	}
	var best ModelInfo
	bestLen := 0
	for _, m := range modelCatalog[providerID] {
		id := strings.ToLower(m.ID)
		if id == norm {
			return m, true
		}
		if strings.HasPrefix(norm, id+"-") && len(id) > bestLen && versionOnlyRemainder(norm[len(id)+1:]) {
			best, bestLen = m, len(id)
		}
	}
	if bestLen > 0 {
		return best, true
	}
	if providerID == azureFoundryProviderID && strings.HasPrefix(norm, "claude") {
		return catalogEntryLookup("claude", served)
	}
	return ModelInfo{}, false
}

// versionOnlyRemainder reports whether what follows a catalogued ID is only a version marker (digits and
// dashes: "2025-04-14", "0324"), never a model-family suffix like "mini".
func versionOnlyRemainder(rest string) bool {
	if rest == "" {
		return false
	}
	for _, r := range rest {
		if (r < '0' || r > '9') && r != '-' && r != '.' {
			return false
		}
	}
	return true
}

// catalogModels returns a fresh copy of a provider's catalogued models so callers
// can't mutate the shared backing array.
func catalogModels(providerID string) []ModelInfo {
	src := modelCatalog[providerID]
	out := make([]ModelInfo, len(src))
	copy(out, src)
	return out
}
