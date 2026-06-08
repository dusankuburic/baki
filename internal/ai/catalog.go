package ai

// modelCatalog is the single source of truth for the models each provider
// serves, with their pricing and limits. This is the data that drifts as vendors
// ship new models and change prices; centralizing it here keeps it in one
// auditable place, separate from the stable per-provider request/SSE code, so a
// model refresh is a one-file edit. TestProviderCatalogConsistency validates that
// each provider's DefaultModel/FreeModel resolve against these entries.
//
// The "demo" provider builds its model list dynamically and is intentionally not
// catalogued here.
var modelCatalog = map[string][]ModelInfo{
	"claude": {
		{ID: "claude-opus-4-8", DisplayName: "Claude Opus 4.8", ContextLimit: 1000000, MaxOutputTokens: 128000, Pricing: Pricing{InputCostPerM: 5.0, OutputCostPerM: 25.0}},
		{ID: "claude-sonnet-4-6", DisplayName: "Claude Sonnet 4.6", ContextLimit: 1000000, MaxOutputTokens: 64000, Pricing: Pricing{InputCostPerM: 3.0, OutputCostPerM: 15.0}},
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
		{ID: "glm-5.1", DisplayName: "GLM-5.1", ContextLimit: 200000, Pricing: Pricing{InputCostPerM: 1.4, OutputCostPerM: 4.4}},
		{ID: "glm-5", DisplayName: "GLM-5", ContextLimit: 200000, Pricing: Pricing{InputCostPerM: 1.0, OutputCostPerM: 3.2}},
		{ID: "glm-5-turbo", DisplayName: "GLM-5 Turbo", ContextLimit: 200000, Pricing: Pricing{InputCostPerM: 0.8, OutputCostPerM: 2.4}},
		{ID: "glm-4.7", DisplayName: "GLM-4.7", ContextLimit: 200000, Pricing: Pricing{InputCostPerM: 0.6, OutputCostPerM: 2.2}},
		{ID: "glm-4.7-flashx", DisplayName: "GLM-4.7 FlashX", ContextLimit: 200000, Pricing: Pricing{InputCostPerM: 0.2, OutputCostPerM: 0.6}},
		{ID: "glm-4.7-flash", DisplayName: "GLM-4.7 Flash (Free)", ContextLimit: 200000, Pricing: Pricing{InputCostPerM: 0, OutputCostPerM: 0}},
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
}

// catalogModels returns a fresh copy of a provider's catalogued models so callers
// can't mutate the shared backing array.
func catalogModels(providerID string) []ModelInfo {
	src := modelCatalog[providerID]
	out := make([]ModelInfo, len(src))
	copy(out, src)
	return out
}
