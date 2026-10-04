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

// Azure AI Foundry model registry (Part 9): a user or organisation keeps several Foundry resources, each with
// its own credential document and a registry of the models on it (chat/Claude deployments, project agents,
// embedding deployments, with purpose, description and optional window/output overrides). This file is the
// provider side: KeyStore keys, "m/<entryId>" model ids, validation, and the per-resource provider whose
// listing the entries drive (NewAzureFoundryConnectionProvider; RefreshAzureFoundryListing fills its cache).

// Registry model kinds: chat is a /openai/v1 deployment, claude the Anthropic route (azure_claude.go), agent a
// project agent (the Responses route), embedding the knowledge base's embedder — never a chat target.
const (
	AzureModelKindChat      = "chat"
	AzureModelKindClaude    = "claude"
	AzureModelKindAgent     = "agent"
	AzureModelKindEmbedding = "embedding"
)

// AzureDefaultConnectionID is the resource the legacy single document became; its credential keeps the
// historical KeyStore key "azure-foundry", so a rolled-back binary still finds it (new ids are generated).
const AzureDefaultConnectionID = "default"

// azureConnectionKeyPrefix starts the KeyStore key of every resource but the default: "azure-foundry/<connId>".
// The storage family helpers match an exact key or this prefix — never a LIKE pattern.
const azureConnectionKeyPrefix = azureFoundryProviderID + "/"

// registryModelPrefix starts a registry entry's model id, "m/<entryId>". "/" cannot occur in a deployment name
// (azureDeploymentName), so the id never collides with the raw deployment, "claude:" and "agent:" ids.
const registryModelPrefix = "m/"

// azureRegistryID is the shape of a generated resource or entry id: ten lower-case base-36 characters (never "default").
var azureRegistryID = regexp.MustCompile(`^[a-z0-9]{10}$`)

// Bounds of an entry's overrides (the registry's CHECK constraints). The provider ignores a value outside them.
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

// ValidAzureConnectionID reports whether id can name a resource: "default" (the legacy document) or a
// generated ten-character id. It is what keeps a KeyStore key built from it inside the family.
func ValidAzureConnectionID(id string) bool {
	return id == AzureDefaultConnectionID || azureRegistryID.MatchString(id)
}

// AzureConnectionKey is the KeyStore key of one resource's credential document: "azure-foundry" for the
// default resource (the legacy key, so a rollback keeps working), "azure-foundry/<connId>" for every other.
func AzureConnectionKey(connID string) string {
	if connID == AzureDefaultConnectionID {
		return azureFoundryProviderID
	}
	return azureConnectionKeyPrefix + connID
}

// ParseAzureConnectionKey is AzureConnectionKey's inverse: the resource id a key stores, false for a key
// outside the family or with an impossible id ("azure-foundry/default" included).
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

// ParseRegistryModelID returns the entry id of an "m/<entryId>" model id; false for anything else.
func ParseRegistryModelID(id string) (entryID string, ok bool) {
	entryID, found := strings.CutPrefix(id, registryModelPrefix)
	if !found || !azureRegistryID.MatchString(entryID) {
		return "", false
	}
	return entryID, true
}

// ValidAzureDeploymentName reports whether name is a valid deployment name (chat, Claude and embedding
// entries): letters, digits, '.', '_' and '-', at most 64 — the rule the credential document follows.
func ValidAzureDeploymentName(name string) bool { return azureDeploymentName.MatchString(name) }

// ValidAzureAgentName reports whether name can be an agent entry's name: 1 to 128 printable characters without
// surrounding space, each allowed in registry text, at least one visible — the name travels in a JSON body and
// a path-escaped URL, so control, bidi and zero-width characters are refused.
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

// AzureTextRuneAllowed reports whether r may appear in registry text users write and members read (names,
// purposes, descriptions). Refused: control/format characters (bidi controls, zero-width), other
// default-ignorables, variation selectors, braille blank, private-use, surrogates and unassigned — what
// renders as nothing or differently on every system, letting one shared name impersonate another or show blank.
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

// azureBrailleBlank is U+2800 BRAILLE PATTERN BLANK: a symbol (So) that renders as a blank cell.
const azureBrailleBlank = 0x2800

// AzureTextRuneVisible reports whether r shows as something: an allowed letter, number, punctuation mark or
// symbol. Spaces and combining marks alone are not visible: every name needs one visible character.
func AzureTextRuneVisible(r rune) bool {
	return AzureTextRuneAllowed(r) && unicode.In(r, unicode.L, unicode.N, unicode.P, unicode.S)
}

// AzureModelTarget is the provider-level model id an entry of kind resolves to: the deployment for chat,
// "claude:<deployment>" for Claude, "agent:<name>" for an agent; false for embedding or an unknown kind.
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

// AzureRegisteredModel is what the provider needs of one registry entry. The display name, purpose and
// description stay in the registry: the provider sizes and routes requests, the service presents them.
type AzureRegisteredModel struct {
	// EntryID is the registry entry's id ("m/<EntryID>" is its model id).
	EntryID string
	// Kind is one of the AzureModelKind* constants.
	Kind string
	// Deployment is the deployment name (chat, claude, embedding) or the agent's name (agent).
	Deployment string
	// ContextWindow and MaxOutputTokens are the user's overrides (0 = none): they win over the catalogue,
	// listing and served-model memo, while a learned window (the deployment's own rejection) still clamps them.
	ContextWindow   int
	MaxOutputTokens int
}

// ModelID is the entry's "m/<entryId>" model id.
func (m AzureRegisteredModel) ModelID() string { return RegistryModelID(m.EntryID) }

// Target is the provider-level id the entry resolves to (AzureModelTarget);
// false for an embedding entry.
func (m AzureRegisteredModel) Target() (string, bool) { return AzureModelTarget(m.Kind, m.Deployment) }

// ValidateAzureRegisteredModel checks one entry as the provider will use it: a known kind, a valid deployment
// or agent name, overrides inside their bounds (none on an embedding entry); *AzureConfigError failures.
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

// ProjectAccess reports whether the document reaches its Foundry project's data plane: a project endpoint
// with Entra ID or CLI sign-in (the project routes reject API keys). Agents need it, and so does discovery.
func (c AzureFoundryConfig) ProjectAccess() bool { return c.listsProjectDeployments() }

// hasDeploymentFields reports whether the document names any deployment in the legacy fields.
func (c AzureFoundryConfig) hasDeploymentFields() bool {
	return c.ChatDeployment != "" || c.EmbeddingDeployment != "" || len(c.Deployments) > 0 || len(c.ClaudeDeployments) > 0
}

// withoutDeployments is the document without the legacy deployment fields: a resource's models are its registry
// entries, and the default resource's document keeps those fields only so a rolled-back binary still reads them.
func (c AzureFoundryConfig) withoutDeployments() AzureFoundryConfig {
	c.ChatDeployment, c.EmbeddingDeployment = "", ""
	c.Deployments, c.ClaudeDeployments = nil, nil
	return c
}

// ValidateAzureConnection validates one resource's document against the host policy WITHOUT the rule that it
// name a chat deployment (a resource's models are registry entries, and it may have none yet).
// refuseDeploymentFields refuses legacy deployment fields — only the default resource keeps them (rollback).
func ValidateAzureConnection(cfg AzureFoundryConfig, refuseDeploymentFields bool) error {
	if err := cfg.Validate(currentAzureHostPolicy()); err != nil {
		return err
	}
	if refuseDeploymentFields && cfg.hasDeploymentFields() {
		return azureCfgErr(AzureCfgErrInvalidConfig, "a resource's configuration names no deployments — add them to the resource's models instead")
	}
	return nil
}

// ValidateAzureConnectionConfig is ValidateAzureConnection for a stored document, parsed strictly first
// (unknown field or oversized document refused); zero config with a parse failure.
func ValidateAzureConnectionConfig(raw string, refuseDeploymentFields bool) (AzureFoundryConfig, error) {
	cfg, err := ParseAzureFoundryConfig(raw)
	if err != nil {
		return AzureFoundryConfig{}, err
	}
	return cfg, ValidateAzureConnection(cfg, refuseDeploymentFields)
}

// NewAzureFoundryConnectionProvider builds the provider for one registry resource: raw is its credential
// document, models its FULL entry set (any order — the provider sorts it, so picker and chat share one listing
// cache key), embeddingDeployment its knowledge-base embedder ("" for chat). Legacy deployment fields are
// cleared before validation and the lists filled from the entries. Invalid entries are skipped, out-of-bounds
// overrides ignored; the listing never waits on the network; DefaultModel is "" — the registry picks.
func NewAzureFoundryConnectionProvider(raw string, models []AzureRegisteredModel, embeddingDeployment string) Provider {
	return newAzureFoundryConnectionProvider(raw, models, embeddingDeployment, azureHTTPClient(), processEntraSources)
}

// newAzureFoundryConnectionProvider parses and validates the document, then builds the registry provider on client with the given Entra ID sources.
func newAzureFoundryConnectionProvider(raw string, models []AzureRegisteredModel, embeddingDeployment string, client *http.Client, sources entraSourceFactory) *AzureFoundryProvider {
	cfg, err := ParseAzureFoundryConfig(raw)
	if err == nil {
		// Cleared before Validate: the caps and names of the legacy fields belong to the single-document release.
		cfg = cfg.withoutDeployments()
		err = cfg.Validate(currentAzureHostPolicy())
	}
	return newAzureRegistryProvider(cfg, err, models, embeddingDeployment, client, sources)
}

// newAzureRegistryProvider is the registry provider's constructor proper; tests hand it a parsed (unvalidated) config.
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

// sortedAzureRegistry is the provider's view of an entry set: valid entries only (out-of-bounds overrides
// dropped), sorted by kind then deployment name, one entry per deployment namespace (chat and Claude share
// one — the chat entry wins) — so the listing cache key is independent of the caller's order.
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

// registryModel is one registry entry as the audited wrapper applies it to a listing: the entry the provider
// lists for it when the live listing lacks it (info, whose ID is the entry's provider-level id), and the user's overrides.
type registryModel struct {
	info            ModelInfo
	contextWindow   int
	maxOutputTokens int
}

// registryModelSource is implemented by providers built for a model registry; the audited wrapper adopts it
// from the raw provider (adoptRawHints — the wrappers in between hide optional methods) and applies it in Models.
type registryModelSource interface {
	registryModels() []registryModel
}

// registryModels satisfies registryModelSource (nil for a document provider): each entry's fallback listing
// entry is the one the provider builds for its kind, sized from the catalogue where it can.
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

// addUnlistedRegistryModels lists every registry entry the live listing lacks (an agent beyond the cap, a
// failed or partial listing) so sizing finds it; an entry the listing carries in another spelling is listed
// under its own id with that entry's data.
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

// listedIndex is the index of the listing entry with id — exactly, or case-insensitively when exact is false — or -1.
func listedIndex(models []ModelInfo, id string, exact bool) int {
	for i, m := range models {
		if m.ID == id || (!exact && strings.EqualFold(m.ID, id)) {
			return i
		}
	}
	return -1
}

// applyRegistryOverrides applies the entries' overrides to their listing entries by provider-level id, over
// whatever the catalogue, listing or served-model memo said; the learned window still clamps both afterwards.
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

// azureModelsStaleGrace is how long past its expiry a registry provider still serves a resource's cached
// project listing (cachedListing): its listing never waits on the network, and a listing an hour old is still
// the better guess than the undecorated entries.
const azureModelsStaleGrace = time.Hour

// RefreshAzureFoundryListing fills the process cache of one resource's project listing, which the chat path
// and picker read without waiting on it. raw and models are the pair ProviderFactory.ForAzureConnection
// receives, so both key the same listing. Blocks until cached or ctx ends (the fetch still completes,
// detached and bounded); concurrent refreshes share one fetch. A still-live listing, a resource that cannot
// list its project and a failed listing return nil; an invalid document returns ErrProviderConfigInvalid.
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
