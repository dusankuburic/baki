package ai

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"sync"

	"pad-analyzer/internal/capability"
	"pad-analyzer/internal/egress"
	"pad-core/models"
)

// The device-flow providers' ids, for the service that runs their flows (their keystore rows are their specs').
const (
	GitHubModelsProviderID = "github-models"
	CopilotProviderID      = copilotProviderID
)

// ErrUnknownProvider is the factory's answer for an id no spec describes: the client's mistake (a typo, a stale
// client), never a setup failure. Match with errors.Is.
var ErrUnknownProvider = errors.New("unknown provider")

// A ProviderSpec is the one description of an AI provider: its identity, what it can do, where it may run, how
// its credential is entered and stored, and how it is built. Every provider list the backend keeps —
// AvailableProviders, the embedding fallback order, the keystore key names, the alias providers,
// KnownProvider — is derived from the registered specs (one spec_<id>.go per provider), so adding a
// provider is one file plus a catalogue entry, and the golden in testdata/providers.golden shows the change.

// AuthType is the provider's auth type on the wire (ProviderInfo.authType): a stored key or an OAuth flow. The
// document providers (Azure AI Foundry, local) report api_key: the document is user-entered, and the client's
// union has no third value.
type AuthType string

const (
	AuthAPIKey AuthType = "api_key"
	AuthOAuth  AuthType = "oauth"
)

// CredentialKind is how a credential is entered: a bare key through the generic form, the GitHub device flow, a
// JSON document behind the provider's own panel, or nothing.
type CredentialKind string

const (
	CredentialAPIKey      CredentialKind = "api_key"
	CredentialOAuthDevice CredentialKind = "oauth_device" // #nosec G101 -- a credential kind name, not a credential
	CredentialDocument    CredentialKind = "document"
	CredentialNone        CredentialKind = "none"
)

// FieldType is a credential field's input type in the generic form.
type FieldType string

const (
	FieldText   FieldType = "text"
	FieldSecret FieldType = "secret"
	FieldURL    FieldType = "url"
	FieldSelect FieldType = "select"
	FieldNumber FieldType = "number"
	FieldJSON   FieldType = "json"
)

// CredentialField is one input of the credential form.
type CredentialField struct {
	Name        string
	Label       string
	Type        FieldType
	Required    bool
	Placeholder string
	Help        string
	// Options are the choices of a select field.
	Options []SelectOption
}

// SelectOption is one choice of a select field: the value the credential stores and the label a person reads
// (empty: the value itself).
type SelectOption struct {
	Value string
	Label string
}

// CredentialSchema describes how a provider's credential is entered.
type CredentialSchema struct {
	Kind CredentialKind
	// Fields is empty for oauth_device (the flow needs no input) and none.
	Fields []CredentialField
	// HelpURL is the vendor page where the credential is obtained.
	HelpURL string
}

// Cap is a provider capability; a spec carries a set of them.
type Cap uint32

const (
	CapChat Cap = 1 << iota
	CapStream
	// CapEmbeddings: Embed works and the provider names its embedding identity.
	CapEmbeddings
	// CapTools: the Chat path serializes tool definitions and returns tool calls.
	CapTools
	// CapReasoning: the request's ReasoningEffort reaches the model.
	CapReasoning
	// CapModelAliases: model ids are user-chosen aliases (deployment names); the served model is memoised and
	// the provider is unpriced.
	CapModelAliases
	// CapLiveListing: Models() asks the vendor rather than the catalogue.
	CapLiveListing
	// CapOrgShared: an organisation may hold a shared credential members fall back to.
	CapOrgShared
	// CapPromptCache: the provider bills prompt-cache reads and writes at their own rates.
	CapPromptCache
)

var capNames = []struct {
	c    Cap
	name string
}{
	{CapChat, "chat"}, {CapStream, "stream"}, {CapEmbeddings, "embeddings"}, {CapTools, "tools"},
	{CapReasoning, "reasoning"}, {CapModelAliases, "model_aliases"}, {CapLiveListing, "live_listing"},
	{CapOrgShared, "org_shared"}, {CapPromptCache, "prompt_cache"},
}

// Has reports whether every bit of want is set.
func (c Cap) Has(want Cap) bool { return c&want == want }

// Names lists the set capabilities by their wire names, in bit order.
func (c Cap) Names() []string {
	var out []string
	for _, n := range capNames {
		if c.Has(n.c) {
			out = append(out, n.name)
		}
	}
	return out
}

// WireFamily is the request/response format a provider speaks; metadata for now (the codecs stay per provider).
type WireFamily string

const (
	WireAnthropic WireFamily = "anthropic"
	WireOpenAI    WireFamily = "openai"
	WireGemini    WireFamily = "gemini"
	WireResponses WireFamily = "responses"
)

// EgressKind is where a provider's requests go.
type EgressKind string

const (
	// EgressFixedHost: the vendor's host, a constant in the binary (an operator may override it by environment).
	EgressFixedHost EgressKind = "fixed_host"
	// EgressUserEndpoint: a host the user names in the credential document, checked against the host allow-list.
	EgressUserEndpoint EgressKind = "user_endpoint"
	// EgressLoopback: the user's own machine only.
	EgressLoopback EgressKind = "loopback"
)

// EgressPolicy is the outbound rule of a provider.
type EgressPolicy struct {
	Kind EgressKind
	// DialGuard: connections are checked at dial time against the deployment's egress policy (BlockInternal in
	// the cloud profile); the fixed-host providers trust their constant.
	DialGuard bool
}

// KeySchema names the keystore rows a provider's credential lives in.
type KeySchema struct {
	// Primary is the key of the stored credential: the API key, the PAT, or the JSON document.
	Primary string
	// OAuthToken is the key of the device-flow token when a provider also has one ("" otherwise).
	OAuthToken string
	// Family: the provider also stores "<Primary>/<id>" rows (Azure AI Foundry's resources).
	Family bool
}

// Credential is what a Resolver hands a Ctor: the stored key (an API key, a PAT or a JSON document) or, for a
// device-flow provider, the OAuth token with the exchanger that turns it into a session token.
type Credential struct {
	Key        string
	OAuthToken string
	OAuth      *CopilotAuth
}

// ProviderListing is what a ListingHook contributes to one provider's entry in the listing.
type ProviderListing struct {
	Models           []models.ModelDetail
	DefaultModel     string
	Configured       bool
	Shared           bool
	SuggestDiscovery bool
}

// ListingHook is implemented by a provider family whose configured state and model listing live outside the
// keystore and the catalogue (Azure AI Foundry's model registry). nil: the keystore keys say whether the
// provider is configured and the catalogue or a live listing supplies the models.
type ListingHook interface {
	Configured(ctx context.Context, scope string) (bool, error)
	OrgConfigured(ctx context.Context, orgID string) (bool, error)
	Picker(ctx context.Context, scope, orgID string) (ProviderListing, error)
}

// ProviderSpec describes one provider; see the package comment above.
type ProviderSpec struct {
	ID       string
	Name     string
	AuthType AuthType
	Caps     Cap
	// Modes are the deployment profiles the provider is offered in.
	Modes      []capability.Profile
	Egress     EgressPolicy
	Credential CredentialSchema
	Keys       KeySchema
	// Catalog is the modelCatalog key ("" = no catalogue: the models are whatever the user's server serves).
	Catalog string
	// DefaultModel is the model offered first.
	DefaultModel string
	Wire         WireFamily
	// Resolver, when set, reads the credential itself (Copilot's OAuth-token-then-PAT rule), always the member's own:
	// it is not given the builder's member/org rule, so a spec with a Resolver may not be CapOrgShared
	// (mustRegisterSpec refuses it; every org builder then collapses onto the member's key, and ForOrgCredential
	// reports ErrKeyNotConfigured before the Resolver runs). nil reads Keys.Primary under the builder's rule.
	Resolver func(f *ProviderFactory, scope, orgID string) (cred Credential, memoScope, usageOrg string, err error)
	// Ctor builds the raw provider; Ctor(Credential{}) is the metadata provider.
	Ctor func(Credential) Provider
	// Listing, when set, answers configured state and the picker listing for the family.
	Listing ListingHook
	// Color and Icon are the generic panel's accent and glyph.
	Color string
	Icon  string
	// Order is the display position; lower first.
	Order int
}

var (
	// providerMu guards the registry against RegisterProvider racing a lookup.
	providerMu sync.RWMutex
	// providerSpecs is the registry: the specs registered at init, by id.
	providerSpecs = map[string]*ProviderSpec{}
	// providerOverrides are the test replacements (RegisterProvider): looked up before the registry, never listed.
	providerOverrides = map[string]*ProviderSpec{}
	// aliasModelProviders is derived at registration from CapModelAliases (the audited wrapper and the learned
	// windows index it directly).
	aliasModelProviders = map[string]bool{}
)

// mustRegisterSpec adds a spec at init; a malformed or duplicate one is a programming error.
func mustRegisterSpec(spec ProviderSpec) {
	switch {
	case spec.ID == "":
		panic("ai: provider spec without an id")
	case spec.Ctor == nil:
		panic("ai: provider spec " + spec.ID + " has no Ctor")
	case spec.Keys.Primary == "":
		panic("ai: provider spec " + spec.ID + " names no keystore key")
	case spec.Order <= 0:
		panic("ai: provider spec " + spec.ID + " has no display order")
	case spec.Resolver != nil && spec.Caps.Has(CapOrgShared):
		// resolveCredential hands a Resolver neither the member/org rule nor ForOrgCredential's org-only refusal
		// (R-provider-30): an org-shareable Resolver provider would run an admin's org-only test, or embed an org
		// corpus, on a member's credential.
		panic("ai: provider spec " + spec.ID + " has a Resolver and CapOrgShared: a Resolver does not apply the member/org credential rule")
	}
	providerMu.Lock()
	defer providerMu.Unlock()
	if _, dup := providerSpecs[spec.ID]; dup {
		panic("ai: provider spec " + spec.ID + " registered twice")
	}
	providerSpecs[spec.ID] = &spec
	if spec.Caps.Has(CapModelAliases) {
		aliasModelProviders[spec.ID] = true
	}
}

// lookupSpec finds the spec a builder uses: a test override, else the registered one.
func lookupSpec(id string) (*ProviderSpec, bool) {
	providerMu.RLock()
	defer providerMu.RUnlock()
	if s, ok := providerOverrides[id]; ok {
		return s, true
	}
	s, ok := providerSpecs[id]
	return s, ok
}

// copySpec is a copy whose slices a caller can edit without touching the registry.
func copySpec(s *ProviderSpec) ProviderSpec {
	out := *s
	out.Modes = append([]capability.Profile(nil), s.Modes...)
	out.Credential.Fields = append([]CredentialField(nil), s.Credential.Fields...)
	return out
}

// Specs returns the registered providers in display order (copies).
func Specs() []ProviderSpec {
	providerMu.RLock()
	out := make([]ProviderSpec, 0, len(providerSpecs))
	for _, s := range providerSpecs {
		out = append(out, copySpec(s))
	}
	providerMu.RUnlock()
	sort.Slice(out, func(i, j int) bool { return out[i].Order < out[j].Order })
	return out
}

// SpecOf returns the spec of a provider id (a copy), false for an unknown id.
func SpecOf(id string) (ProviderSpec, bool) {
	s, ok := lookupSpec(id)
	if !ok {
		return ProviderSpec{}, false
	}
	return copySpec(s), true
}

// KnownProvider reports whether id names a provider the factory can build.
func KnownProvider(id string) bool {
	_, ok := lookupSpec(id)
	return ok
}

// InstallListing sets (nil: clears) the Listing hook of a registered provider at runtime — a family's registry
// is a service built after init, so DI installs it at boot. A test override of the same id gets it too. false
// when no spec has the id.
func InstallListing(id string, hook ListingHook) bool {
	providerMu.Lock()
	defer providerMu.Unlock()
	spec, ok := providerSpecs[id]
	if !ok {
		spec, ok = providerOverrides[id]
	}
	if !ok {
		return false
	}
	spec.Listing = hook
	if o, ok := providerOverrides[id]; ok {
		o.Listing = hook
	}
	return true
}

// ProviderOptions is the deployment's outbound policy, applied at boot to every spec by its EgressPolicy
// (ConfigureProviders).
type ProviderOptions struct {
	// Egress is the policy of the user-named endpoints: egress.BlockInternal in the cloud profile, else
	// egress.AllowLoopback (which is also what lets the loopback provider exist at all).
	Egress egress.Policy
	// ExtraAllowedHosts extends a user-endpoint provider's host allow-list (PAD_AZURE_FOUNDRY_ALLOWED_HOSTS).
	ExtraAllowedHosts []string
	// AllowPrivateEgress relaxes a guarded user-endpoint provider to egress.AllowPrivateNetwork (private link).
	AllowPrivateEgress bool
}

// ConfigureProviders applies opts to every registered spec by its EgressPolicy: a user-endpoint provider gets
// the dial-time guard and the allow-list (Azure AI Foundry's process runtime), a loopback provider the gate
// that keeps a cloud server off its own loopback services, and a fixed-host provider a guarded clone of the
// shared client under the same policy — its host is a constant in the binary (an operator's *_API_URL override
// is trusted), so there the guard is defense-in-depth against a misconfiguration or a redirect, never the
// boundary. One call at boot, in place of a call per provider.
func ConfigureProviders(opts ProviderOptions) {
	// Once, not once per spec: every fixed-host provider shares one client.
	policy := opts.Egress
	if opts.AllowPrivateEgress {
		policy = egress.AllowPrivateNetwork
	}
	ConfigureFixedHostClient(policy)
	for _, spec := range Specs() {
		switch spec.Egress.Kind {
		case EgressUserEndpoint:
			ConfigureAzureFoundry(AzureFoundryOptions{
				Egress:             opts.Egress,
				ExtraAllowedHosts:  opts.ExtraAllowedHosts,
				AllowPrivateEgress: opts.AllowPrivateEgress,
			})
		case EgressLoopback:
			ConfigureLocalModels(LocalModelsOptions{Egress: opts.Egress})
		}
	}
}

// WireModes is Modes as the wire carries them.
func (s ProviderSpec) WireModes() []string {
	out := make([]string, 0, len(s.Modes))
	for _, m := range s.Modes {
		out = append(out, string(m))
	}
	return out
}

// WireCredential is the credential schema as the wire carries it (models.CredentialSchema): Fields is never
// null, and a select's options carry a label (the value itself where a spec names none).
func (s ProviderSpec) WireCredential() models.CredentialSchema {
	out := models.CredentialSchema{Kind: string(s.Credential.Kind), Fields: []models.CredentialField{}, HelpURL: s.Credential.HelpURL}
	for _, f := range s.Credential.Fields {
		wf := models.CredentialField{Name: f.Name, Label: f.Label, Type: string(f.Type), Required: f.Required, Placeholder: f.Placeholder, Help: f.Help}
		for _, o := range f.Options {
			label := o.Label
			if label == "" {
				label = o.Value
			}
			wf.Options = append(wf.Options, models.CredentialOption{Value: o.Value, Label: label})
		}
		out.Fields = append(out.Fields, wf)
	}
	return out
}

// RegisterProvider replaces a provider's constructor, or adds a stub provider, for the duration of a test; it is
// not a plugin API (production code never calls it: internal/archtest pins that). A stub gets the capabilities
// the factory tests exercise — chat, embeddings, tools and the org fallback — and is built, never listed.
func RegisterProvider(id string, ctor func(key string) Provider) {
	providerMu.Lock()
	defer providerMu.Unlock()
	spec := ProviderSpec{
		ID: id, Name: id, AuthType: AuthAPIKey,
		Caps:       CapChat | CapStream | CapEmbeddings | CapTools | CapOrgShared,
		Modes:      []capability.Profile{capability.Desktop, capability.LocalDB, capability.Cloud},
		Egress:     EgressPolicy{Kind: EgressFixedHost},
		Credential: CredentialSchema{Kind: CredentialAPIKey},
		Keys:       KeySchema{Primary: id},
		Wire:       WireOpenAI,
		Order:      1 << 30,
	}
	if builtin, ok := providerSpecs[id]; ok {
		spec = copySpec(builtin)
	}
	spec.Resolver = nil
	spec.Ctor = func(c Credential) Provider { return ctor(c.Key) }
	providerOverrides[id] = &spec
}

// allProfiles is Modes for a provider offered everywhere.
var allProfiles = []capability.Profile{capability.Desktop, capability.LocalDB, capability.Cloud}

// apiKeyCredential is the one-field schema of a bare-key provider. The field is named as endpoints.save
// (/api/keys/save) reads it — {provider, key} — so a client that posts the catalogue's fields by name works.
func apiKeyCredential(placeholder, helpURL string) CredentialSchema {
	return CredentialSchema{
		Kind:    CredentialAPIKey,
		Fields:  []CredentialField{{Name: "key", Label: "API key", Type: FieldSecret, Required: true, Placeholder: placeholder}},
		HelpURL: helpURL,
	}
}

func unknownProviderError(id string) error { return fmt.Errorf("%w: %s", ErrUnknownProvider, id) }
