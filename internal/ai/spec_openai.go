package ai

// openaiProviderID is the registry id: the constructors label their counters with it (a reference to openaiSpec.ID from a
// constructor would be an initialization cycle through Ctor).
const openaiProviderID = "openai"

// openaiSpec: OpenAI's chat completions and embeddings with a bare key.
var openaiSpec = ProviderSpec{
	ID:           openaiProviderID,
	Name:         "OpenAI",
	AuthType:     AuthAPIKey,
	Caps:         CapChat | CapStream | CapEmbeddings | CapTools | CapReasoning | CapOrgShared,
	Modes:        allProfiles,
	Egress:       EgressPolicy{Kind: EgressFixedHost},
	Credential:   apiKeyCredential("sk-...", "https://platform.openai.com/api-keys"),
	Keys:         KeySchema{Primary: "openai"},
	Catalog:      "openai",
	DefaultModel: "gpt-4o",
	Wire:         WireOpenAI,
	Ctor:         func(c Credential) Provider { return NewOpenAIProvider(c.Key) },
	Color:        "#10a37f",
	Icon:         "cloud",
	Order:        20,
}

func init() { mustRegisterSpec(openaiSpec) }
