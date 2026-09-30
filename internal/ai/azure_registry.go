package ai

import (
	"context"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"pad-core/logger"
)

// Azure AI Foundry model registry (Part 9). A user — or an organisation —
// keeps several Foundry RESOURCES ("connections" in code, "resources" in the
// UI), each with its own endpoint and credential document, and a registry of
// the models on each one: chat and Claude deployments, project agents and
// embedding deployments, each with a purpose, a description and optional
// context-window and max-output overrides. The registry itself lives in the
// service's store; this file is the provider side of it:
//   - the KeyStore key each resource's credential document is stored under;
//   - the "m/<entryId>" model ids the chat picker sends, and the
//     provider-level id each entry resolves to ("<deployment>",
//     "claude:<deployment>", "agent:<name>");
//   - validation of a resource's credential document and of an entry;
//   - the provider built for one resource and its entries
//     (NewAzureFoundryConnectionProvider), whose listing the entries drive and
//     which never waits on the network for it (RefreshAzureFoundryListing
//     fills the cache it reads).

// Registry model kinds. A chat entry is a chat-completions deployment
// (/openai/v1), a claude entry a Claude deployment (the Anthropic Messages
// route, azure_claude.go), an agent entry a project agent (the Responses
// route, azure_agents.go) and an embedding entry an embedding deployment —
// the knowledge base's embedder, never a chat target.
const (
	AzureModelKindChat      = "chat"
	AzureModelKindClaude    = "claude"
	AzureModelKindAgent     = "agent"
	AzureModelKindEmbedding = "embedding"
)

// AzureDefaultConnectionID is the resource the legacy single document became.
// Its credential keeps the historical KeyStore key "azure-foundry", so a
// binary rolled back to the single-document release still finds it; a new
// resource never gets this id (generated ids are azureRegistryID's ten
// characters).
const AzureDefaultConnectionID = "default"

// azureConnectionKeyPrefix starts the KeyStore key of every resource but the
// default one: "azure-foundry/<connId>". The storage family helpers match a
// family as the exact key or this prefix — never a LIKE pattern — so
// "azure-foundryX" is never a member.
const azureConnectionKeyPrefix = azureFoundryProviderID + "/"

// registryModelPrefix starts a registry entry's model id, "m/<entryId>". "/"
// cannot occur in a deployment name (azureDeploymentName), so the id never
// collides with the raw deployment, "claude:" and "agent:" ids a request can
// still carry for one release.
const registryModelPrefix = "m/"

// azureRegistryID is the shape of a generated resource or entry id: ten
// lower-case base-36 characters (never "default").
var azureRegistryID = regexp.MustCompile(`^[a-z0-9]{10}$`)

// Bounds of an entry's overrides (the registry's CHECK constraints). The
// provider ignores a value outside them rather than size a request by it.
const (
	AzureContextWindowMin   = 1024
	AzureContextWindowMax   = 2_000_000
	AzureMaxOutputTokensMin = 256
	AzureMaxOutputTokensMax = 1_000_000
)

// azureAgentNameMax bounds an agent entry's name, in characters.
const azureAgentNameMax = 128

// Registry validation failure codes, next to the document codes of
// azure_foundry_config.go (same *AzureConfigError envelope).
const (
	AzureCfgErrInvalidKind          = "INVALID_KIND"
	AzureCfgErrInvalidContextWindow = "INVALID_CONTEXT_WINDOW"
	AzureCfgErrInvalidMaxOutput     = "INVALID_MAX_OUTPUT"
)

// ValidAzureModelKind reports whether kind is one of the four registry kinds.
func ValidAzureModelKind(kind string) bool {
	switch kind {
	case AzureModelKindChat, AzureModelKindClaude, AzureModelKindAgent, AzureModelKindEmbedding:
		return true
	}
	return false
}

// ValidAzureConnectionID reports whether id can name a resource: "default"
// (the legacy document) or a generated ten-character id. It is what keeps a
// KeyStore key built from it (AzureConnectionKey) inside the family.
func ValidAzureConnectionID(id string) bool {
	return id == AzureDefaultConnectionID || azureRegistryID.MatchString(id)
}

// AzureConnectionKey is the KeyStore key of one resource's credential
// document: "azure-foundry" for the default resource (the legacy key, so a
// rollback keeps working), "azure-foundry/<connId>" for every other one. The
// caller validates connID (ValidAzureConnectionID); the factory refuses an
// invalid one before building a key from it.
func AzureConnectionKey(connID string) string {
	if connID == AzureDefaultConnectionID {
		return azureFoundryProviderID
	}
	return azureConnectionKeyPrefix + connID
}

// ParseAzureConnectionKey is AzureConnectionKey's inverse: the resource id a
// KeyStore key stores, false for a key outside the family or with an id no
// resource can have ("azure-foundry/default" included — the default
// resource's key is the bare family name).
func ParseAzureConnectionKey(key string) (connID string, ok bool) {
	if key == azureFoundryProviderID {
		return AzureDefaultConnectionID, true
	}
	id, found := strings.CutPrefix(key, azureConnectionKeyPrefix)
	if !found || !azureRegistryID.MatchString(id) {
		return "", false
	}
	return id, true
}

// RegistryModelID is the model id the chat picker sends for a registry entry.
func RegistryModelID(entryID string) string { return registryModelPrefix + entryID }

// ParseRegistryModelID returns the entry id of an "m/<entryId>" model id;
// false for anything else (a raw deployment, "claude:" or "agent:" id, a
// malformed entry id).
func ParseRegistryModelID(id string) (entryID string, ok bool) {
	entryID, found := strings.CutPrefix(id, registryModelPrefix)
	if !found || !azureRegistryID.MatchString(entryID) {
		return "", false
	}
	return entryID, true
}

// ValidAzureDeploymentName reports whether name is a valid deployment name
// (chat, Claude and embedding entries): letters, digits, '.', '_' and '-', at
// most 64 — the rule the credential document's deployment fields follow.
func ValidAzureDeploymentName(name string) bool { return azureDeploymentName.MatchString(name) }

// ValidAzureAgentName reports whether name can be an agent entry's name: 1 to
// 128 printable characters without surrounding space, every one allowed in
// registry text (AzureTextRuneAllowed) and at least one of them visible
// (AzureTextRuneVisible). The name travels in a JSON body (agent_reference)
// and, path-escaped, in the agent lookup URL; control characters, bidi
// controls and zero-width characters (unicode format characters) are not
// printable and are refused, and so is a name that would show as nothing.
func ValidAzureAgentName(name string) bool {
	if name == "" || name != strings.TrimSpace(name) || !utf8.ValidString(name) ||
		utf8.RuneCountInString(name) > azureAgentNameMax {
		return false
	}
	visible := false
	for _, r := range name {
		if !unicode.IsPrint(r) || !AzureTextRuneAllowed(r) {
			return false
		}
		visible = visible || AzureTextRuneVisible(r)
	}
	return visible
}

// AzureTextRuneAllowed reports whether r may appear in registry text that
// users and admins write and members read — a resource name, a model's
// display name, custom purpose and description, an agent name. Refused:
// control and format characters (NUL, bidi controls, zero-width
// characters), the other default-ignorable code points (U+034F, the Hangul
// fillers U+115F, U+1160, U+3164 and U+FFA0, U+FFF0–U+FFF8, …), variation
// selectors, U+2800 BRAILLE PATTERN BLANK (a symbol that draws nothing),
// private-use and surrogate code points, and code points Unicode has not
// assigned: what renders as nothing, or differently on every system, and so
// lets one shared name impersonate another ("Admin" and "Admin" plus an
// invisible character) or show blank.
func AzureTextRuneAllowed(r rune) bool {
	switch {
	case r == azureBrailleBlank:
		return false
	case unicode.In(r, unicode.Cc, unicode.Cf, unicode.Co, unicode.Cs,
		unicode.Other_Default_Ignorable_Code_Point, unicode.Variation_Selector):
		return false
	case !unicode.In(r, unicode.L, unicode.M, unicode.N, unicode.P, unicode.S, unicode.Z):
		return false // unassigned (no general category)
	}
	return true
}

// azureBrailleBlank is U+2800 BRAILLE PATTERN BLANK: a symbol (So) that
// renders as a blank cell.
const azureBrailleBlank = 0x2800

// AzureTextRuneVisible reports whether r shows as something: an allowed
// (AzureTextRuneAllowed) letter, number, punctuation mark or symbol. Spaces
// and combining marks alone are not visible: every name needs one visible
// character, a description one when it is not empty.
func AzureTextRuneVisible(r rune) bool {
	return AzureTextRuneAllowed(r) && unicode.In(r, unicode.L, unicode.N, unicode.P, unicode.S)
}

// AzureModelTarget is the provider-level model id an entry of kind resolves
// to — what the chat request, the listing and every memo carry: the
// deployment name for a chat entry, "claude:<deployment>" for a Claude entry
// (the Anthropic route), "agent:<name>" for an agent. false for an embedding
// entry (never a chat target) and an unknown kind.
func AzureModelTarget(kind, deployment string) (string, bool) {
	if deployment == "" {
		return "", false
	}
	switch kind {
	case AzureModelKindChat:
		return deployment, true
	case AzureModelKindClaude:
		return claudeModelPrefix + deployment, true
	case AzureModelKindAgent:
		return agentModelPrefix + deployment, true
	}
	return "", false
}

// AzureRegisteredModel is what the provider needs of one registry entry. The
// display name, purpose and description stay in the registry: the provider
// sizes and routes requests, the service presents them.
type AzureRegisteredModel struct {
	// EntryID is the registry entry's id ("m/<EntryID>" is its model id).
	EntryID string
	// Kind is one of the AzureModelKind* constants.
	Kind string
	// Deployment is the deployment name (chat, claude, embedding) or the
	// agent's name (agent).
	Deployment string
	// ContextWindow and MaxOutputTokens are the user's overrides (0 = none),
	// for a small distilled model the catalogue and the project listing
	// cannot size: they win over both and over the served-model memo, while a
	// learned window (the deployment's own context-length rejection) still
	// clamps them (auditedProvider.Models). Never set on an embedding entry.
	ContextWindow   int
	MaxOutputTokens int
}

// ModelID is the entry's "m/<entryId>" model id.
func (m AzureRegisteredModel) ModelID() string { return RegistryModelID(m.EntryID) }

// Target is the provider-level id the entry resolves to (AzureModelTarget);
// false for an embedding entry.
func (m AzureRegisteredModel) Target() (string, bool) { return AzureModelTarget(m.Kind, m.Deployment) }

// ValidateAzureRegisteredModel checks one entry as the provider will use it:
// a known kind, a valid deployment or agent name, and overrides inside their
// bounds (none on an embedding entry). Failures are *AzureConfigError values:
// INVALID_KIND, INVALID_DEPLOYMENT, INVALID_CONTEXT_WINDOW, INVALID_MAX_OUTPUT.
// Whether an agent's resource can run it (a project endpoint and Entra ID —
// AzureFoundryConfig.ProjectAccess) is the caller's check.
func ValidateAzureRegisteredModel(m AzureRegisteredModel) error {
	switch m.Kind {
	case AzureModelKindChat, AzureModelKindClaude, AzureModelKindEmbedding:
		if !azureDeploymentName.MatchString(m.Deployment) {
			return azureCfgErr(AzureCfgErrInvalidDeploy, "deployment name %q may only contain letters, digits, '.', '_' and '-' (max 64)", m.Deployment)
		}
	case AzureModelKindAgent:
		if !ValidAzureAgentName(m.Deployment) {
			return azureCfgErr(AzureCfgErrInvalidDeploy, "agent name %q must be 1 to %d printable characters without surrounding spaces, at least one of them visible and none of them invisible (zero-width, bidi, filler) or unassigned", m.Deployment, azureAgentNameMax)
		}
	default:
		return azureCfgErr(AzureCfgErrInvalidKind, "unknown model kind %q (use %q, %q, %q or %q)", m.Kind,
			AzureModelKindChat, AzureModelKindClaude, AzureModelKindAgent, AzureModelKindEmbedding)
	}
	if m.Kind == AzureModelKindEmbedding {
		if m.ContextWindow != 0 {
			return azureCfgErr(AzureCfgErrInvalidContextWindow, "an embedding model has no context window to set")
		}
		if m.MaxOutputTokens != 0 {
			return azureCfgErr(AzureCfgErrInvalidMaxOutput, "an embedding model has no output to cap")
		}
		return nil
	}
	if m.ContextWindow != 0 && !azureContextWindowInRange(m.ContextWindow) {
		return azureCfgErr(AzureCfgErrInvalidContextWindow, "the context window must be between %d and %d tokens", AzureContextWindowMin, AzureContextWindowMax)
	}
	if m.MaxOutputTokens != 0 && !azureMaxOutputInRange(m.MaxOutputTokens) {
		return azureCfgErr(AzureCfgErrInvalidMaxOutput, "the maximum output must be between %d and %d tokens", AzureMaxOutputTokensMin, AzureMaxOutputTokensMax)
	}
	return nil
}

func azureContextWindowInRange(n int) bool {
	return n >= AzureContextWindowMin && n <= AzureContextWindowMax
}

func azureMaxOutputInRange(n int) bool {
	return n >= AzureMaxOutputTokensMin && n <= AzureMaxOutputTokensMax
}

// ProjectAccess reports whether the document reaches its Foundry project's
// data plane: a project endpoint with Microsoft Entra ID or Azure CLI sign-in
// (the project routes reject API keys). Agents need it (an agent entry on a
// resource without it is unsupported) and so does discovery
// (ErrAzureDiscoveryUnavailable).
func (c AzureFoundryConfig) ProjectAccess() bool { return c.listsProjectDeployments() }

// hasDeploymentFields reports whether the document names any deployment in
// the legacy fields.
func (c AzureFoundryConfig) hasDeploymentFields() bool {
	return c.ChatDeployment != "" || c.EmbeddingDeployment != "" || len(c.Deployments) > 0 || len(c.ClaudeDeployments) > 0
}

// withoutDeployments is the document without the legacy deployment fields:
// a resource's models are its registry entries, and the default resource's
// document keeps those fields only so a rolled-back binary still reads them.
func (c AzureFoundryConfig) withoutDeployments() AzureFoundryConfig {
	c.ChatDeployment, c.EmbeddingDeployment = "", ""
	c.Deployments, c.ClaudeDeployments = nil, nil
	return c
}

// ValidateAzureConnection validates one resource's credential document
// against the operator's host policy — endpoint, project endpoint, auth mode
// and secret — WITHOUT the single-document rule that it name a chat
// deployment (MISSING_DEPLOYMENT): a resource's models are registry entries,
// and a resource may have none yet. refuseDeploymentFields refuses a document
// that names deployments in the legacy fields (INVALID_CONFIG): only the
// default resource's document carries them, preserved for a rollback, and
// they are validated as before there (names, list caps).
func ValidateAzureConnection(cfg AzureFoundryConfig, refuseDeploymentFields bool) error {
	if err := cfg.Validate(currentAzureHostPolicy()); err != nil {
		return err
	}
	if refuseDeploymentFields && cfg.hasDeploymentFields() {
		return azureCfgErr(AzureCfgErrInvalidConfig, "a resource's configuration names no deployments — add them to the resource's models instead")
	}
	return nil
}

// ValidateAzureConnectionConfig is ValidateAzureConnection for a stored
// document: it is parsed first (strictly — an unknown field or an oversized
// document is refused, as for the legacy document). The parsed document comes
// back with a validation failure too, zero with a parse failure.
func ValidateAzureConnectionConfig(raw string, refuseDeploymentFields bool) (AzureFoundryConfig, error) {
	cfg, err := ParseAzureFoundryConfig(raw)
	if err != nil {
		return AzureFoundryConfig{}, err
	}
	return cfg, ValidateAzureConnection(cfg, refuseDeploymentFields)
}

// NewAzureFoundryConnectionProvider builds the provider for one resource of
// the model registry: raw is the resource's credential document, models the
// resource's FULL entry set (every kind, in any order — the provider sorts
// it, so the picker and the chat path, which both pass the full set, share
// one listing cache key), embeddingDeployment the knowledge base's embedding
// deployment on it ("" for chat; ProviderFactory.ForAzureConnectionEmbedding
// sets it).
//
// The document's legacy deployment fields are cleared BEFORE it is validated
// and the provider's deployment lists are then filled from the entries, so
// the document's list caps (16 deployments, 20 Claude deployments — sized for
// the 2 KB keyring limit) never apply to a registry. Everything else is the
// document provider's: chat entries answer on /openai/v1, Claude entries as
// "claude:<name>" on the Anthropic route (and a chat entry the cached
// project listing re-lists as a Claude deployment is routed there too), agent
// entries as "agent:<name>" on the Responses route (Entra ID and a project
// endpoint, as before). Entries that fail ValidateAzureRegisteredModel are
// skipped (a registry row the store should never have kept), and overrides
// outside their bounds are ignored.
//
// The listing never waits on the network (Models): it is the entries plus
// the cached project listing, which RefreshAzureFoundryListing fills. The
// audited wrapper lists the entries the listing lacks (agents beyond the
// listing cap, a failed or partial project listing) and applies the
// overrides (auditedProvider.Models), so ContextLimitFor and
// ModelMaxOutputTokens always find a registered entry. DefaultModel is ""
// — the registry, not the provider, picks the default.
func NewAzureFoundryConnectionProvider(raw string, models []AzureRegisteredModel, embeddingDeployment string) Provider {
	return newAzureFoundryConnectionProvider(raw, models, embeddingDeployment, azureHTTPClient(), processEntraSources)
}

// newAzureFoundryConnectionProvider parses and validates the document, then
// builds the registry provider on client with the given Entra ID sources.
func newAzureFoundryConnectionProvider(raw string, models []AzureRegisteredModel, embeddingDeployment string, client *http.Client, sources entraSourceFactory) *AzureFoundryProvider {
	cfg, err := ParseAzureFoundryConfig(raw)
	if err == nil {
		// Cleared before Validate: the caps and names of the legacy fields
		// belong to the single-document release, not to this resource.
		cfg = cfg.withoutDeployments()
		err = cfg.Validate(currentAzureHostPolicy())
	}
	return newAzureRegistryProvider(cfg, err, models, embeddingDeployment, client, sources)
}

// newAzureRegistryProvider is the registry provider's constructor proper;
// tests hand it a parsed (unvalidated) config pointing at a local server.
func newAzureRegistryProvider(cfg AzureFoundryConfig, cfgErr error, models []AzureRegisteredModel, embeddingDeployment string, client *http.Client, sources entraSourceFactory) *AzureFoundryProvider {
	cfg = cfg.withoutDeployments()
	entries := sortedAzureRegistry(models)
	embeddingDeployment = strings.TrimSpace(embeddingDeployment)
	if cfgErr == nil && embeddingDeployment != "" && !azureDeploymentName.MatchString(embeddingDeployment) {
		cfgErr = azureCfgErr(AzureCfgErrInvalidDeploy, "deployment name %q may only contain letters, digits, '.', '_' and '-' (max 64)", embeddingDeployment)
	}
	for _, e := range entries {
		switch e.Kind {
		case AzureModelKindChat:
			cfg.Deployments = append(cfg.Deployments, e.Deployment)
		case AzureModelKindClaude:
			cfg.ClaudeDeployments = append(cfg.ClaudeDeployments, e.Deployment)
		}
	}
	cfg.EmbeddingDeployment = embeddingDeployment
	p := newAzureFoundryProviderFrom(cfg, cfgErr, client, sources)
	p.fromRegistry = true
	p.registry = entries
	return p
}

// sortedAzureRegistry is the provider's view of an entry set: valid entries
// only (trimmed; overrides outside their bounds dropped), sorted by kind,
// then deployment name case-insensitively (then exactly, then by entry id),
// and one entry per deployment name space — chat and Claude entries share
// one (a deployment answers on one route; the chat entry, sorted first,
// wins), agents and embedding deployments have their own — as the registry's
// unique index has it. The order makes the provider's deployment lists, and
// so the listing cache key, independent of the order the caller passed.
func sortedAzureRegistry(models []AzureRegisteredModel) []AzureRegisteredModel {
	out := make([]AzureRegisteredModel, 0, len(models))
	for _, m := range models {
		m.Kind = strings.TrimSpace(m.Kind)
		m.Deployment = strings.TrimSpace(m.Deployment)
		named := m
		named.ContextWindow, named.MaxOutputTokens = 0, 0
		if err := ValidateAzureRegisteredModel(named); err != nil {
			logger.Warn("azure ai foundry: skipping an invalid registry entry", "entry", m.EntryID, "error", err)
			continue
		}
		if m.Kind == AzureModelKindEmbedding || !azureContextWindowInRange(m.ContextWindow) {
			m.ContextWindow = 0
		}
		if m.Kind == AzureModelKindEmbedding || !azureMaxOutputInRange(m.MaxOutputTokens) {
			m.MaxOutputTokens = 0
		}
		out = append(out, m)
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if la, lb := strings.ToLower(a.Deployment), strings.ToLower(b.Deployment); la != lb {
			return la < lb
		}
		if a.Deployment != b.Deployment {
			return a.Deployment < b.Deployment
		}
		return a.EntryID < b.EntryID
	})
	seen := make(map[string]bool, len(out))
	kept := out[:0]
	for _, m := range out {
		ns := m.Kind
		if ns == AzureModelKindClaude {
			ns = AzureModelKindChat
		}
		key := ns + "\x00" + strings.ToLower(m.Deployment)
		if seen[key] {
			continue
		}
		seen[key] = true
		kept = append(kept, m)
	}
	return kept
}

// registryModel is one registry entry as the audited wrapper applies it to a
// listing: the entry the provider lists for it when the live listing lacks it
// (info, whose ID is the entry's provider-level id), and the user's
// overrides (0 = none).
type registryModel struct {
	info            ModelInfo
	contextWindow   int
	maxOutputTokens int
}

// registryModelSource is implemented by providers built for a model registry
// (NewAzureFoundryConnectionProvider). The audited wrapper adopts it from the
// raw provider (adoptRawHints) — the resilience and tracing wrappers in
// between hide optional methods — and applies it in Models.
type registryModelSource interface {
	registryModels() []registryModel
}

// registryModels satisfies registryModelSource: nil for a document provider.
// Each entry's fallback listing entry is the one the provider itself builds
// for its kind: a chat or embedding deployment sized from the catalogue when
// named after a catalogued model (deploymentEntry), a Claude deployment at
// the Claude defaults (claudeDeploymentEntry), an agent at the provider-wide
// window until the served-model memo knows its model.
func (p *AzureFoundryProvider) registryModels() []registryModel {
	if !p.fromRegistry {
		return nil
	}
	out := make([]registryModel, 0, len(p.registry))
	for _, e := range p.registry {
		var info ModelInfo
		switch e.Kind {
		case AzureModelKindChat:
			info = p.deploymentEntry(e.Deployment, false)
		case AzureModelKindClaude:
			info = claudeDeploymentEntry(e.Deployment)
		case AzureModelKindAgent:
			info = ModelInfo{ID: agentModelPrefix + e.Deployment, DisplayName: "Agent · " + e.Deployment, ContextLimit: p.ContextLimit()}
		case AzureModelKindEmbedding:
			info = p.deploymentEntry(e.Deployment, true)
		default:
			continue
		}
		out = append(out, registryModel{info: info, contextWindow: e.ContextWindow, maxOutputTokens: e.MaxOutputTokens})
	}
	return out
}

// addUnlistedRegistryModels lists every registry entry the live listing
// lacks — an agent beyond the listing's cap, anything a failed or partial
// project listing left out, every agent while no project listing is cached —
// so ContextLimitFor and ModelMaxOutputTokens find it: a listing failure
// never drops a registered entry. An entry the listing carries in another
// spelling, or a chat entry it re-lists as a Claude deployment
// ("claude:<name>", routed there by relistedClaude), is listed under the
// entry's own id with that listing entry's data — the id the chat request
// carries. Runs before the served-model memo enriches the listing, so an
// unlisted entry learns its model like a listed one.
func addUnlistedRegistryModels(models []ModelInfo, registry []registryModel) []ModelInfo {
	for _, r := range registry {
		id := r.info.ID
		if listedIndex(models, id, true) >= 0 {
			continue
		}
		i := listedIndex(models, id, false)
		if i < 0 && !r.info.Embedding && !IsAgentModel(id) && !IsClaudeModel(id) {
			i = listedIndex(models, claudeModelPrefix+id, false)
		}
		if i >= 0 {
			m := models[i]
			m.ID, m.Embedding = id, r.info.Embedding
			models = append(models, m)
			continue
		}
		models = append(models, r.info)
	}
	return models
}

// listedIndex is the index of the listing entry with id — exactly, or
// case-insensitively when exact is false — or -1.
func listedIndex(models []ModelInfo, id string, exact bool) int {
	for i, m := range models {
		if m.ID == id || (!exact && strings.EqualFold(m.ID, id)) {
			return i
		}
	}
	return -1
}

// applyRegistryOverrides applies the entries' overrides to their listing
// entries, by provider-level id: the context window becomes ContextLimit and
// the max output MaxOutputTokens, over whatever the catalogue, the project
// listing or the served-model memo said — the user sized a model those cannot
// know. The learned window still clamps both afterwards
// (applyLearnedWindows). Embedding entries carry no overrides; a listing
// entry of the same id flagged as an embedding deployment (the project
// listing's opinion of a chat entry's deployment) is resized like any other,
// since the chat request still names that id.
func applyRegistryOverrides(models []ModelInfo, registry []registryModel) []ModelInfo {
	type override struct{ window, output int }
	byID := make(map[string]override, len(registry))
	for _, r := range registry {
		if r.contextWindow <= 0 && r.maxOutputTokens <= 0 {
			continue
		}
		if _, dup := byID[r.info.ID]; !dup {
			byID[r.info.ID] = override{r.contextWindow, r.maxOutputTokens}
		}
	}
	if len(byID) == 0 {
		return models
	}
	for i := range models {
		o, ok := byID[models[i].ID]
		if !ok {
			continue
		}
		if o.window > 0 {
			models[i].ContextLimit = o.window
		}
		if o.output > 0 {
			models[i].MaxOutputTokens = o.output
		}
	}
	return models
}

// azureModelsStaleGrace is how long past its expiry a registry provider
// still serves a resource's cached project listing (cachedListing). Its
// listing never waits on the network, so an expired listing would otherwise
// leave the chat path with the undecorated entries — the catalogue and the
// memo only, no Claude re-listing — until the next refresh; a listing an
// hour old is still the better guess. The refresh on the read paths
// (RefreshAzureFoundryListing) replaces it.
const azureModelsStaleGrace = time.Hour

// RefreshAzureFoundryListing fills the process cache of one resource's
// project listing — the project's deployments (and the model behind each)
// and agents — which the chat path and the model picker read without ever
// waiting on it. raw is the resource's saved credential document and models
// its full entry set: the set ProviderFactory.ForAzureConnection receives,
// so both key the same listing. It blocks until the listing is cached, or
// ctx ends (the fetch itself then still completes, detached and bounded, and
// fills the cache for the next reader). Concurrent refreshes of one resource
// — and a document provider's listing of the same key — share one fetch. A
// listing that is still live, and a resource that cannot list its project
// (an API key, no project endpoint), cost nothing and return nil; so does a
// project that failed to list (the entries keep working undecorated, and the
// failure is logged). An invalid document is returned as the provider's
// ErrProviderConfigInvalid error.
func RefreshAzureFoundryListing(ctx context.Context, raw string, models []AzureRegisteredModel) error {
	return newAzureFoundryConnectionProvider(raw, models, "", azureHTTPClient(), processEntraSources).refreshListing(ctx)
}

// refreshListing is RefreshAzureFoundryListing for a built provider.
func (p *AzureFoundryProvider) refreshListing(ctx context.Context) error {
	if p.configErr != nil {
		return p.configErr
	}
	if p.isolated || !p.cfg.Configured() || !p.cfg.listsProjectDeployments() {
		return nil
	}
	key := azureModelsCacheKey(p.cfg)
	if _, live := cachedAzureModels(key); live {
		return nil
	}
	select {
	case <-azureModelsFlight.DoChan(key, p.listingFlight(ctx, key)):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
