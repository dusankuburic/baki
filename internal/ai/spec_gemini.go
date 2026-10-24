package ai

// geminiSpec: Google's generateContent API with a bare key; embeddings through batchEmbedContents.
var geminiSpec = ProviderSpec{
	ID:           "gemini",
	Name:         "Gemini",
	AuthType:     AuthAPIKey,
	Caps:         CapChat | CapStream | CapEmbeddings | CapTools | CapOrgShared,
	Modes:        allProfiles,
	Egress:       EgressPolicy{Kind: EgressFixedHost},
	Credential:   apiKeyCredential("AIza...", "https://aistudio.google.com/apikey"),
	Keys:         KeySchema{Primary: "gemini"},
	Catalog:      "gemini",
	DefaultModel: "gemini-2.5-pro",
	Wire:         WireGemini,
	Ctor:         func(c Credential) Provider { return NewGeminiProvider(c.Key) },
	Color:        "#4285f4",
	Icon:         "cloud",
	Order:        30,
}

func init() { mustRegisterSpec(geminiSpec) }
