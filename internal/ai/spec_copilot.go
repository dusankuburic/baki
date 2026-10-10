package ai

import "fmt"

// The Copilot keystore rows: the manual PAT under the provider id, the device-flow token next to it.
const (
	copilotProviderID    = "copilot"
	copilotOAuthTokenKey = "copilot-oauth-token" // #nosec G101 -- a keystore key NAME, not a credential
)

// copilotSpec: GitHub Copilot's chat endpoint. Dual auth — the device-flow token (exchanged for a session token
// per call) first, else a manual PAT — read by copilotResolver; a subscription is a person's, so the
// credential is never an organisation's (no CapOrgShared). Covered by the subscription: nothing is metered.
var copilotSpec = ProviderSpec{
	ID:       copilotProviderID,
	Name:     "GitHub Copilot",
	AuthType: AuthOAuth,
	Caps:     CapChat | CapStream | CapReasoning | CapLiveListing,
	Modes:    allProfiles,
	Egress:   EgressPolicy{Kind: EgressFixedHost},
	Credential: CredentialSchema{
		Kind:    CredentialOAuthDevice,
		Fields:  []CredentialField{{Name: "pat", Label: "Personal access token", Type: FieldSecret, Placeholder: "ghp_...", Help: "Instead of signing in: a fine-grained token with Copilot access."}},
		HelpURL: "https://github.com/settings/tokens",
	},
	Keys:         KeySchema{Primary: copilotProviderID, OAuthToken: copilotOAuthTokenKey},
	Catalog:      "copilot",
	DefaultModel: "gpt-4o",
	Pricing:      PricingFree,
	Wire:         WireOpenAI,
	Resolver:     copilotResolver,
	Ctor: func(c Credential) Provider {
		if c.OAuthToken != "" && c.OAuth != nil {
			return NewCopilotProviderWithAuth(c.OAuth, c.OAuthToken)
		}
		return NewCopilotProvider(c.Key)
	},
	Color: "#6e40c9",
	Icon:  "github",
	Order: 70,
}

// copilotResolver reads the per-user OAuth token (the device flow's) when the factory can exchange it, else the
// manual PAT; both are the caller's own, so the memo scope is theirs and no organisation is billed.
func copilotResolver(f *ProviderFactory, scope, _ string) (Credential, string, string, error) {
	oauthToken, oauthErr := f.getKey(scope, copilotOAuthTokenKey)
	if oauthErr == nil && oauthToken != "" && f.copilotAuth != nil {
		return Credential{OAuthToken: oauthToken, OAuth: f.copilotAuth}, scope, "", nil
	}
	pat, patErr := f.getKey(scope, copilotProviderID)
	if patErr == nil && pat != "" {
		return Credential{Key: pat}, scope, "", nil
	}
	return Credential{}, "", "", fmt.Errorf("%s not configured: no OAuth token or PAT found", copilotProviderID)
}

func init() { mustRegisterSpec(copilotSpec) }
