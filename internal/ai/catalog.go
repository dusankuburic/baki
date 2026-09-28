package ai

import (
	"regexp"
	"strings"
)

// modelCatalog is the single source of truth for the models each provider
// serves, with their pricing and limits. This is the data that drifts as vendors
// ship new models and change prices; centralizing it here keeps it in one
// auditable place, separate from the stable per-provider request/SSE code, so a
// model refresh is a one-file edit. TestProviderCatalogConsistency validates that
// each provider's DefaultModel/FreeModel resolve against these entries.
var modelCatalog = map[string][]ModelInfo{
	// Claude: the Claude API models that Microsoft Foundry also sells (its
	// "API model IDs and deployments" table), so a Foundry Claude deployment
	// is priced by the model that answers. Base input/output USD per 1M
	// tokens, context window and synchronous max output as published on
	// platform.claude.com (about-claude/pricing and each models/<id>/overview
	// page), checked 2026-09-28; Foundry bills the same per-model rates in
	// Claude Consumption Units. Claude Opus 4.1 ($15/$75) is retired except on
	// Bedrock and Google Cloud — neither the Claude API nor Foundry serves it —
	// so it is not listed.
	"claude": {
		{ID: "claude-fable-5-1", DisplayName: "Claude Fable 5.1", ContextLimit: 1000000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 10.0, OutputCostPerM: 50.0}},
		{ID: "claude-fable-5", DisplayName: "Claude Fable 5", ContextLimit: 1000000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 10.0, OutputCostPerM: 50.0}},
		{ID: "claude-mythos-5-1", DisplayName: "Claude Mythos 5.1", ContextLimit: 1000000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 10.0, OutputCostPerM: 50.0}},
		{ID: "claude-mythos-5", DisplayName: "Claude Mythos 5", ContextLimit: 1000000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 10.0, OutputCostPerM: 50.0}},
		{ID: "claude-opus-5-5", DisplayName: "Claude Opus 5.5", ContextLimit: 1000000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 4.0, OutputCostPerM: 20.0}},
		{ID: "claude-opus-5", DisplayName: "Claude Opus 5", ContextLimit: 1000000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 5.0, OutputCostPerM: 25.0}},
		{ID: "claude-opus-4-8", DisplayName: "Claude Opus 4.8", ContextLimit: 1000000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 5.0, OutputCostPerM: 25.0}},
		{ID: "claude-opus-4-7", DisplayName: "Claude Opus 4.7", ContextLimit: 1000000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 5.0, OutputCostPerM: 25.0}},
		{ID: "claude-opus-4-6", DisplayName: "Claude Opus 4.6", ContextLimit: 1000000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 5.0, OutputCostPerM: 25.0}},
		{ID: "claude-opus-4-5", DisplayName: "Claude Opus 4.5", ContextLimit: 200000, MaxOutputTokens: 64000, Pricing: Pricing{InputCostPerM: 5.0, OutputCostPerM: 25.0}},
		{ID: "claude-sonnet-5-5", DisplayName: "Claude Sonnet 5.5", ContextLimit: 1000000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 2.0, OutputCostPerM: 10.0}},
		{ID: "claude-sonnet-5", DisplayName: "Claude Sonnet 5", ContextLimit: 1000000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 2.0, OutputCostPerM: 10.0}},
		{ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6", ContextLimit: 1000000, MaxOutputTokens: 64000, Pricing: Pricing{InputCostPerM: 3.0, OutputCostPerM: 15.0}},
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
		// glm-5.3 and glm-5.2 verified against the z.ai coding endpoint
		// 2026-09-23; the -turbo/-air/-flash variants of either do not exist
		// there (1211 Unknown Model). The 4.7 family was dropped from this
		// deployment's catalog on request — glm-4.7-flashx is additionally
		// not entitled on GLM Coding Plan keys (1113 resource-package error).
		{ID: "glm-5.3", DisplayName: "GLM-5.3", ContextLimit: 200000, Pricing: Pricing{InputCostPerM: 1.4, OutputCostPerM: 4.4}},
		{ID: "glm-5.2", DisplayName: "GLM-5.2", ContextLimit: 200000, Pricing: Pricing{InputCostPerM: 1.4, OutputCostPerM: 4.4}},
		{ID: "glm-5.1", DisplayName: "GLM-5.1", ContextLimit: 200000, Pricing: Pricing{InputCostPerM: 1.4, OutputCostPerM: 4.4}},
		{ID: "glm-5", DisplayName: "GLM-5", ContextLimit: 200000, Pricing: Pricing{InputCostPerM: 1.0, OutputCostPerM: 3.2}},
		{ID: "glm-5-turbo", DisplayName: "GLM-5 Turbo", ContextLimit: 200000, Pricing: Pricing{InputCostPerM: 0.8, OutputCostPerM: 2.4}},
	},
	// GitHub Models' free tier enforces an 8 192-token request limit for most
	// models regardless of native context window; the limits cap the context
	// budget so the API never returns a 413.
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
	// Azure AI Foundry serves whatever the user DEPLOYED, under a name the
	// user chose, so this list is not what the provider offers — it is the
	// reference table the served-model name (response "model") is priced and
	// sized against: Azure global-standard list prices (USD per 1M tokens,
	// 2026-09) and the models' INPUT caps. ContextLimit is what the chat
	// service truncates the prompt against, so where Azure documents separate
	// input and output limits (gpt-5 family: 272k in + 128k out = 400k) the
	// input figure goes here, never the total window. The unconfigured
	// (metadata) provider also advertises it so the UI has something to show
	// before the first deployment listing.
	"azure-foundry": {
		{ID: "gpt-4.1", DisplayName: "GPT-4.1", ContextLimit: 1047576, MaxOutputTokens: 32768, Pricing: Pricing{InputCostPerM: 2.0, OutputCostPerM: 8.0}},
		{ID: "gpt-4.1-mini", DisplayName: "GPT-4.1 Mini", ContextLimit: 1047576, MaxOutputTokens: 32768, Pricing: Pricing{InputCostPerM: 0.4, OutputCostPerM: 1.6}},
		{ID: "gpt-4.1-nano", DisplayName: "GPT-4.1 Nano", ContextLimit: 1047576, MaxOutputTokens: 32768, Pricing: Pricing{InputCostPerM: 0.1, OutputCostPerM: 0.4}},
		{ID: "gpt-4o", DisplayName: "GPT-4o", ContextLimit: 128000, MaxOutputTokens: 16384, Pricing: Pricing{InputCostPerM: 2.5, OutputCostPerM: 10.0}},
		{ID: "gpt-4o-mini", DisplayName: "GPT-4o Mini", ContextLimit: 128000, MaxOutputTokens: 16384, Pricing: Pricing{InputCostPerM: 0.15, OutputCostPerM: 0.6}},
		{ID: "gpt-5", DisplayName: "GPT-5", ContextLimit: 272000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 1.25, OutputCostPerM: 10.0}},
		{ID: "gpt-5-mini", DisplayName: "GPT-5 Mini", ContextLimit: 272000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 0.25, OutputCostPerM: 2.0}},
		{ID: "gpt-5-nano", DisplayName: "GPT-5 Nano", ContextLimit: 272000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 0.05, OutputCostPerM: 0.4}},
		{ID: "gpt-5-chat", DisplayName: "GPT-5 Chat", ContextLimit: 128000, MaxOutputTokens: 16384, Pricing: Pricing{InputCostPerM: 1.25, OutputCostPerM: 10.0}},
		{ID: "o3", DisplayName: "o3", ContextLimit: 200000, MaxOutputTokens: 100000, Pricing: Pricing{InputCostPerM: 2.0, OutputCostPerM: 8.0}},
		{ID: "o4-mini", DisplayName: "o4-mini", ContextLimit: 200000, MaxOutputTokens: 100000, Pricing: Pricing{InputCostPerM: 1.1, OutputCostPerM: 4.4}},
		{ID: "gpt-oss-120b", DisplayName: "gpt-oss 120B", ContextLimit: 131072, MaxOutputTokens: 32768, Pricing: Pricing{InputCostPerM: 0.15, OutputCostPerM: 0.6}},
		// model-router bills by the model it routes to; that model is what the
		// response reports, so the router entry itself carries no price.
		{ID: "model-router", DisplayName: "Model router", ContextLimit: 128000, Pricing: Pricing{}},
		{ID: "DeepSeek-V3-0324", DisplayName: "DeepSeek V3", ContextLimit: 131072, MaxOutputTokens: 32768, Pricing: Pricing{InputCostPerM: 1.14, OutputCostPerM: 4.56}},
		{ID: "DeepSeek-V3.1", DisplayName: "DeepSeek V3.1", ContextLimit: 131072, MaxOutputTokens: 32768, Pricing: Pricing{InputCostPerM: 1.14, OutputCostPerM: 4.56}},
		{ID: "DeepSeek-R1-0528", DisplayName: "DeepSeek R1", ContextLimit: 131072, MaxOutputTokens: 32768, Pricing: Pricing{InputCostPerM: 1.35, OutputCostPerM: 5.4}},
		{ID: "Llama-4-Maverick-17B-128E-Instruct-FP8", DisplayName: "Llama 4 Maverick", ContextLimit: 1000000, MaxOutputTokens: 32768, Pricing: Pricing{InputCostPerM: 0.25, OutputCostPerM: 0.97}},
		{ID: "grok-3", DisplayName: "Grok 3", ContextLimit: 131072, MaxOutputTokens: 32768, Pricing: Pricing{InputCostPerM: 3.0, OutputCostPerM: 15.0}},
		{ID: "grok-3-mini", DisplayName: "Grok 3 Mini", ContextLimit: 131072, MaxOutputTokens: 32768, Pricing: Pricing{InputCostPerM: 0.25, OutputCostPerM: 1.27}},
		{ID: "grok-4", DisplayName: "Grok 4", ContextLimit: 256000, MaxOutputTokens: 32768, Pricing: Pricing{InputCostPerM: 3.0, OutputCostPerM: 15.0}},
		{ID: "Phi-4", DisplayName: "Phi-4", ContextLimit: 16384, MaxOutputTokens: 8192, Pricing: Pricing{InputCostPerM: 0.125, OutputCostPerM: 0.5}},
		{ID: "text-embedding-3-small", DisplayName: "text-embedding-3-small", ContextLimit: 8191, Pricing: Pricing{InputCostPerM: 0.02, OutputCostPerM: 0}},
		{ID: "text-embedding-3-large", DisplayName: "text-embedding-3-large", ContextLimit: 8191, Pricing: Pricing{InputCostPerM: 0.13, OutputCostPerM: 0}},
		{ID: "text-embedding-ada-002", DisplayName: "text-embedding-ada-002", ContextLimit: 8191, Pricing: Pricing{InputCostPerM: 0.1, OutputCostPerM: 0}},
	},
}

// claudeInviteOnly lists catalogued Claude models offered by invitation only
// (Project Glasswing): kept in the catalogue so a Foundry deployment of one
// is priced and sized, but not offered in the direct provider's picker
// (ClaudeProvider.Models), where almost every key would be refused.
var claudeInviteOnly = map[string]bool{
	"claude-mythos-5-1": true,
	"claude-mythos-5":   true,
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

// catalogEntryFor finds the catalogue entry describing the model a provider
// reported serving. The match is exact on the normalised name, or — for
// names that only add a numeric/date version to a catalogued ID
// ("gpt-4.1-2025-04-14", "deepseek-v3-0324-2", "claude-sonnet-4-6-20260301")
// — the longest catalogued ID the name extends. Anything else (a sibling
// model such as "gpt-5-mini" when only "gpt-5" is catalogued) is NOT matched:
// mispricing silently would be worse than the explicit fallback.
//
// Azure AI Foundry's Claude deployments are priced by the Claude catalogue:
// a "claude:<deployment>" id resolves by its deployment name there, and a
// served model named "claude-…" (a Claude deployment's answer, or a model
// router's route to one) that the Azure table does not list is looked up
// there too.
func catalogEntryFor(providerID, served string) (ModelInfo, bool) {
	if providerID == azureFoundryProviderID && IsClaudeModel(served) {
		return catalogEntryFor("claude", claudeDeploymentName(served))
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
		return catalogEntryFor("claude", served)
	}
	return ModelInfo{}, false
}

// versionOnlyRemainder reports whether what follows a catalogued ID is only a
// version marker (digits and dashes: "2025-04-14", "0324", "2"), never a
// model-family suffix like "mini".
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
