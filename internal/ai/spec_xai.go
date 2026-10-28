package ai

// xaiProviderID is the registry id: the constructors label their counters with it (a reference to xaiSpec.ID from a
// constructor would be an initialization cycle through Ctor).
const xaiProviderID = "xai"

// xaiSpec: xAI's OpenAI-compatible endpoint with a bare key; chat only.
var xaiSpec = ProviderSpec{
	ID:           xaiProviderID,
	Name:         "xAI (Grok)",
	AuthType:     AuthAPIKey,
	Caps:         CapChat | CapStream | CapTools | CapReasoning | CapOrgShared,
	Modes:        allProfiles,
	Egress:       EgressPolicy{Kind: EgressFixedHost},
	Credential:   apiKeyCredential("xai-...", "https://console.x.ai/"),
	Keys:         KeySchema{Primary: "xai"},
	Catalog:      "xai",
	DefaultModel: "grok-3-mini",
	Wire:         WireOpenAI,
	Ctor:         func(c Credential) Provider { return NewXAIProvider(c.Key) },
	Color:        "#f43f5e",
	Icon:         "cloud",
	Order:        40,
}

func init() { mustRegisterSpec(xaiSpec) }
