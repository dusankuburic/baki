package ai

// githubModelsProviderID is the registry id: the constructors label their counters with it (a reference to githubModelsSpec.ID from a
// constructor would be an initialization cycle through Ctor).
const githubModelsProviderID = "github-models"

// githubModelsSpec: GitHub Models through the device flow; the token is stored under its historical key
// "github-models-token", and the listing comes from the service. Free tier: nothing is metered.
var githubModelsSpec = ProviderSpec{
	ID:           githubModelsProviderID,
	Name:         "GitHub Models",
	AuthType:     AuthOAuth,
	Caps:         CapChat | CapStream | CapEmbeddings | CapTools | CapReasoning | CapLiveListing | CapOrgShared,
	Modes:        allProfiles,
	Egress:       EgressPolicy{Kind: EgressFixedHost},
	Credential:   CredentialSchema{Kind: CredentialOAuthDevice, HelpURL: "https://github.com/marketplace/models"},
	Keys:         KeySchema{Primary: "github-models-token"},
	Catalog:      "github-models",
	DefaultModel: "gpt-4o",
	Wire:         WireOpenAI,
	Ctor:         func(c Credential) Provider { return NewGitHubModelsProvider(c.Key) },
	Color:        "#8b5cf6",
	Icon:         "github",
	Order:        60,
}

func init() { mustRegisterSpec(githubModelsSpec) }
