package ai

// claudeSpec: Anthropic's Messages API with a bare key; the only provider billing prompt-cache reads and
// writes at their own rates (cacheReadMultiplier).
var claudeSpec = ProviderSpec{
	ID:           "claude",
	Name:         "Claude",
	AuthType:     AuthAPIKey,
	Caps:         CapChat | CapStream | CapTools | CapReasoning | CapOrgShared | CapPromptCache,
	Modes:        allProfiles,
	Egress:       EgressPolicy{Kind: EgressFixedHost},
	Credential:   apiKeyCredential("sk-ant-...", "https://console.anthropic.com/settings/keys"),
	Keys:         KeySchema{Primary: "claude"},
	Catalog:      "claude",
	DefaultModel: "claude-sonnet-4-6",
	Pricing:      PricingCatalog,
	Wire:         WireAnthropic,
	Ctor:         func(c Credential) Provider { return NewClaudeProvider(c.Key) },
	Color:        "#d4a574",
	Icon:         "cloud",
	Order:        10,
}

func init() { mustRegisterSpec(claudeSpec) }
