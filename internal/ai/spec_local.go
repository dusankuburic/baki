package ai

import "pad-analyzer/internal/capability"

// localSpec: a locally hosted OpenAI-compatible server (Foundry Local, Ollama, LM Studio), described by a JSON
// document naming a LOOPBACK base URL (LocalConfig). Desktop and local-mode only — a cloud server must not be
// pointed at its own loopback services (ConfigureLocalModels keeps the gate at build time too). No catalogue:
// the models are whatever the server serves; nothing is metered.
var localSpec = ProviderSpec{
	ID:       localProviderID,
	Name:     "Local model (OpenAI-compatible)",
	AuthType: AuthAPIKey,
	Caps:     CapChat | CapStream | CapTools | CapReasoning | CapLiveListing,
	Modes:    []capability.Profile{capability.Desktop, capability.LocalDB},
	Egress:   EgressPolicy{Kind: EgressLoopback, DialGuard: true},
	Credential: CredentialSchema{
		Kind: CredentialDocument,
		Fields: []CredentialField{
			{Name: "baseUrl", Label: "Base URL", Type: FieldURL, Required: true, Placeholder: "http://127.0.0.1:11434"},
			{Name: "contextWindow", Label: "Context window", Type: FieldNumber, Help: "The context length the server runs its models with; empty assumes 8192."},
		},
	},
	Keys:    KeySchema{Primary: localProviderID},
	Pricing: PricingFree,
	Wire:    WireOpenAI,
	Ctor:    func(c Credential) Provider { return NewLocalProvider(c.Key) },
	Color:   "#f59e0b",
	Icon:    "drive",
	Order:   90,
}

func init() { mustRegisterSpec(localSpec) }
