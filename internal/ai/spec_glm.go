package ai

// glmSpec: z.ai's OpenAI-compatible endpoint with a bare key.
var glmSpec = ProviderSpec{
	ID:           "glm",
	Name:         "GLM (z.ai)",
	AuthType:     AuthAPIKey,
	Caps:         CapChat | CapStream | CapEmbeddings | CapTools | CapReasoning | CapOrgShared,
	Modes:        allProfiles,
	Egress:       EgressPolicy{Kind: EgressFixedHost},
	Credential:   apiKeyCredential("", "https://z.ai/manage-apikey/apikey-list"),
	Keys:         KeySchema{Primary: "glm"},
	Catalog:      "glm",
	DefaultModel: "glm-5.2",
	Pricing:      PricingCatalog,
	Wire:         WireOpenAI,
	Ctor:         func(c Credential) Provider { return NewGLMProvider(c.Key) },
	Color:        "#06b6d4",
	Icon:         "cloud",
	Order:        50,
}

func init() { mustRegisterSpec(glmSpec) }
