package ai

// glmProviderID is the registry id: the constructors label their counters with it (a reference to glmSpec.ID from a
// constructor would be an initialization cycle through Ctor).
const glmProviderID = "glm"

// glmSpec: z.ai's OpenAI-compatible endpoint with a bare key.
var glmSpec = ProviderSpec{
	ID:           glmProviderID,
	Name:         "GLM (z.ai)",
	AuthType:     AuthAPIKey,
	Caps:         CapChat | CapStream | CapEmbeddings | CapTools | CapReasoning | CapOrgShared,
	Modes:        allProfiles,
	Egress:       EgressPolicy{Kind: EgressFixedHost},
	Credential:   apiKeyCredential("", "https://z.ai/manage-apikey/apikey-list"),
	Keys:         KeySchema{Primary: "glm"},
	Catalog:      "glm",
	DefaultModel: "glm-5.2",
	Wire:         WireOpenAI,
	Ctor:         func(c Credential) Provider { return NewGLMProvider(c.Key) },
	Color:        "#06b6d4",
	Icon:         "cloud",
	Order:        50,
}

func init() { mustRegisterSpec(glmSpec) }
