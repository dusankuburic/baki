package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
)

// Registry models: create, update, delete, the designations (the
// knowledge-base embedding model, an organisation's "Default for members")
// and the validation of what users and admins write into them.

// ---- Text ----

// cleanAzureText normalises user- and admin-written text: NFC, surrounding
// whitespace trimmed, inner whitespace runs (newlines and tabs included)
// collapsed to one space — every name, purpose and description is shown on
// one line.
func cleanAzureText(s string) string {
	if !utf8.ValidString(s) {
		return s // refused by validAzureText
	}
	return strings.Join(strings.Fields(norm.NFC.String(s)), " ")
}

// validAzureText reports whether raw, cleaned to clean (cleanAzureText), is
// an acceptable value: valid UTF-8; only characters registry text allows
// (ai.AzureTextRuneAllowed — no control or format characters: NUL, which
// Postgres refuses outright, bidi controls, zero-width characters; no other
// default-ignorable code point, variation selector, private-use, surrogate
// or unassigned code point — what lets a shared name impersonate another or
// show blank); at least one visible character once it is not empty
// (ai.AzureTextRuneVisible: a name of Hangul fillers or braille blanks shows
// as nothing — a description may be empty, never blank); and minLen–maxLen
// characters (runes, as Postgres char_length counts, never bytes).
func validAzureText(raw, clean string, minLen, maxLen int) bool {
	if !utf8.ValidString(raw) {
		return false
	}
	n, visible := 0, false
	for _, r := range clean {
		if !ai.AzureTextRuneAllowed(r) {
			return false
		}
		visible = visible || ai.AzureTextRuneVisible(r)
		n++
	}
	if n > 0 && !visible {
		return false
	}
	return n >= minLen && n <= maxLen
}

// truncateRunes shortens s to at most n characters.
func truncateRunes(s string, n int) string {
	if utf8.RuneCountInString(s) <= n {
		return s
	}
	return string([]rune(s)[:n])
}

// ---- Purposes ----

// azurePurposeLabels are the presets' English labels: custom text naming a
// preset by its key or its label (case-insensitively) is stored as the
// preset, so "Documentation" typed as a custom purpose groups with the
// preset's models.
var azurePurposeLabels = map[string]string{
	storageif.AzurePurposeGeneral:       "General",
	storageif.AzurePurposeDocumentation: "Documentation",
	storageif.AzurePurposeFixes:         "Fixes & debugging",
	storageif.AzurePurposeReview:        "Review & analysis",
	storageif.AzurePurposeRefactoring:   "Refactoring",
	storageif.AzurePurposeTesting:       "Testing",
}

func isAzurePurposePreset(p string) bool {
	_, ok := azurePurposeLabels[p]
	return ok
}

// matchAzurePurposePreset is the preset custom text names, "" when none.
func matchAzurePurposePreset(text string) string {
	for _, p := range storageif.AzurePurposePresets {
		if strings.EqualFold(text, p) || strings.EqualFold(text, azurePurposeLabels[p]) {
			return p
		}
	}
	return ""
}

// normalizeAzurePurpose returns the stored (purpose, purposeCustom) pair for
// a client's: a preset key (any case) clears the custom text; "custom" takes
// the text — NFC-normalised, trimmed, inner whitespace collapsed, 1–32
// characters, and stored as the preset it names when it names one; an empty
// pair is General; a custom text alone means "custom".
func normalizeAzurePurpose(purpose, custom string) (string, string, error) {
	p := strings.ToLower(strings.TrimSpace(purpose))
	if p == "" {
		if strings.TrimSpace(custom) == "" {
			return storageif.AzurePurposeGeneral, "", nil
		}
		p = storageif.AzurePurposeCustom
	}
	if p != storageif.AzurePurposeCustom {
		if isAzurePurposePreset(p) {
			return p, "", nil
		}
		return "", "", azureRegErr(http.StatusBadRequest, AzureCodeInvalidPurpose,
			fmt.Sprintf("unknown purpose %q (use general, documentation, fixes, review, refactoring, testing or custom)", truncateRunes(purpose, 40)), nil, nil)
	}
	text := cleanAzureText(custom)
	if preset := matchAzurePurposePreset(text); preset != "" {
		return preset, "", nil
	}
	if !validAzureText(custom, text, 1, storageif.AzurePurposeCustomMaxLen) {
		return "", "", azureRegErr(http.StatusBadRequest, AzureCodeInvalidPurpose,
			fmt.Sprintf("a custom purpose must be 1 to %d characters, at least one of them a visible letter, digit, punctuation mark or symbol, and none of them a control, invisible (zero-width, bidi, filler) or unassigned character", storageif.AzurePurposeCustomMaxLen), nil, nil)
	}
	return storageif.AzurePurposeCustom, text, nil
}

// ---- Model fields ----

// azureModelFields are a model's editable fields, validated and normalised
// in place by validateModelFields.
type azureModelFields struct {
	ConnectionID    string
	Kind            string
	Deployment      string
	DisplayName     string
	Purpose         string
	PurposeCustom   string
	Description     string
	ContextWindow   int
	MaxOutputTokens int
}

// validateModelFields checks and normalises f: a known kind (INVALID_KIND),
// a deployment or agent name (INVALID_DEPLOYMENT), overrides inside their
// bounds and none on an embedding model (INVALID_CONTEXT_WINDOW,
// INVALID_MAX_OUTPUT), a display name (defaulting to the deployment name,
// INVALID_DISPLAY_NAME), a purpose (already normalised by the caller) and a
// description (INVALID_DESCRIPTION). The stricter ai validators run first;
// the store's CHECK mirror stays the backstop.
func validateModelFields(f *azureModelFields) error {
	f.Kind = strings.TrimSpace(f.Kind)
	f.Deployment = strings.TrimSpace(f.Deployment)
	if !ai.ValidAzureConnectionID(f.ConnectionID) {
		return azureRegErr(http.StatusNotFound, AzureCodeConnectionNotFound, "the resource was not found", storageif.ErrAzureConnectionNotFound, nil)
	}
	if err := ai.ValidateAzureRegisteredModel(ai.AzureRegisteredModel{
		Kind: f.Kind, Deployment: f.Deployment, ContextWindow: f.ContextWindow, MaxOutputTokens: f.MaxOutputTokens,
	}); err != nil {
		return err
	}
	name := f.DisplayName
	if strings.TrimSpace(name) == "" {
		name = truncateRunes(f.Deployment, storageif.AzureNameMaxLen)
	}
	clean := cleanAzureText(name)
	if !validAzureText(name, clean, 1, storageif.AzureNameMaxLen) {
		return azureRegErr(http.StatusBadRequest, AzureCodeInvalidDisplayName,
			fmt.Sprintf("the display name must be 1 to %d characters, at least one of them a visible letter, digit, punctuation mark or symbol, and none of them a control, invisible (zero-width, bidi, filler) or unassigned character", storageif.AzureNameMaxLen), nil, nil)
	}
	f.DisplayName = clean
	desc := cleanAzureText(f.Description)
	if !validAzureText(f.Description, desc, 0, storageif.AzureDescriptionMaxLen) {
		return azureRegErr(http.StatusBadRequest, AzureCodeInvalidDescription,
			fmt.Sprintf("the description must be at most %d characters and, when set, show at least one visible character, with no control, invisible (zero-width, bidi, filler) or unassigned character", storageif.AzureDescriptionMaxLen), nil, nil)
	}
	f.Description = desc
	return nil
}

// ---- Inputs ----

// OptionalInt is a number field of a partial update that tells its three
// states apart: absent (Set false — keep the stored value), null (Null —
// clear the override) and a value.
type OptionalInt struct {
	Set   bool
	Null  bool
	Value int
}

// UnmarshalJSON records that the field was present, and whether it was
// null. encoding/json calls it for a present field only — null included.
func (o *OptionalInt) UnmarshalJSON(b []byte) error {
	o.Set = true
	if strings.TrimSpace(string(b)) == "null" {
		o.Null, o.Value = true, 0
		return nil
	}
	o.Null = false
	return json.Unmarshal(b, &o.Value)
}

// MarshalJSON writes the value, or null when absent or cleared.
func (o OptionalInt) MarshalJSON() ([]byte, error) {
	if !o.Set || o.Null {
		return []byte("null"), nil
	}
	return json.Marshal(o.Value)
}

// SetOptionalInt is an OptionalInt holding v.
func SetOptionalInt(v int) OptionalInt { return OptionalInt{Set: true, Value: v} }

// NullOptionalInt is an OptionalInt that clears the value.
func NullOptionalInt() OptionalInt { return OptionalInt{Set: true, Null: true} }

// apply is the field's new value given the stored one.
func (o OptionalInt) apply(stored int) int {
	switch {
	case !o.Set:
		return stored
	case o.Null:
		return 0
	}
	return o.Value
}

// AzureModelInput is POST /models.
type AzureModelInput struct {
	ConnectionID string `json:"connectionId"`
	Kind         string `json:"kind"`
	Deployment   string `json:"deployment"`
	// DisplayName defaults to the deployment name; Purpose to general.
	DisplayName   string `json:"displayName,omitempty"`
	Purpose       string `json:"purpose,omitempty"`
	PurposeCustom string `json:"purposeCustom,omitempty"`
	Description   string `json:"description,omitempty"`
	// ContextWindow and MaxOutputTokens are optional overrides (0 = none;
	// never on an embedding model).
	ContextWindow   int `json:"contextWindow,omitempty"`
	MaxOutputTokens int `json:"maxOutputTokens,omitempty"`
	// IsDefault (org scope only) makes it the organisation's "Default for
	// members"; IsKnowledgeEmbedding (an embedding model, where the knowledge
	// base embeds with Azure) the knowledge base's model.
	IsDefault            bool `json:"isDefault,omitempty"`
	IsKnowledgeEmbedding bool `json:"isKnowledgeEmbedding,omitempty"`
	// Force confirms the re-index a new knowledge-base model needs on a
	// non-empty corpus (else 409 REINDEX_REQUIRED).
	Force bool `json:"force,omitempty"`
}

// AzureModelPatch is PUT /models/{id}: an absent field keeps its stored
// value. The id is kept whatever changes — resource, kind, deployment.
type AzureModelPatch struct {
	ConnectionID *string `json:"connectionId,omitempty"`
	Kind         *string `json:"kind,omitempty"`
	Deployment   *string `json:"deployment,omitempty"`
	DisplayName  *string `json:"displayName,omitempty"`
	// Purpose: a preset clears the stored custom text; "custom" takes
	// PurposeCustom (or keeps the stored text).
	Purpose       *string `json:"purpose,omitempty"`
	PurposeCustom *string `json:"purposeCustom,omitempty"`
	// Description: "" clears it.
	Description *string `json:"description,omitempty"`
	// ContextWindow and MaxOutputTokens: null clears the override. On the
	// wire an integer or null (the swaggertype tags say so to the API docs:
	// OptionalInt is how the decoder tells absent, null and a value apart).
	ContextWindow   OptionalInt `json:"contextWindow" swaggertype:"primitive,integer" extensions:"x-nullable"`
	MaxOutputTokens OptionalInt `json:"maxOutputTokens" swaggertype:"primitive,integer" extensions:"x-nullable"`
	// IsDefault / IsKnowledgeEmbedding: true moves the designation to this
	// model, false clears it from it.
	IsDefault            *bool `json:"isDefault,omitempty"`
	IsKnowledgeEmbedding *bool `json:"isKnowledgeEmbedding,omitempty"`
	// UpdatedAt, when sent, is the model's updatedAt as last read
	// (compare-and-swap: a stale value is 409 CONFLICT).
	UpdatedAt string `json:"updatedAt,omitempty"`
	// Force confirms the re-index a knowledge-base change needs on a
	// non-empty corpus (else 409 REINDEX_REQUIRED).
	Force bool `json:"force,omitempty"`
}

// ---- Designations and the corpus ----

// designationRules checks a model's designations against its kind, the
// scope and the deployment: the knowledge-base model is an embedding model,
// offered only where the knowledge base embeds with Azure; the default for
// members is an organisation's chat, Claude or agent model.
func (s *AzureModelService) designationRules(owner storageif.AzureOwner, kind string, kb, def bool) error {
	switch {
	case kb && !s.knowledgeEmbeddingEnabled:
		return azureRegErr(http.StatusBadRequest, AzureCodeInvalidDesignation,
			"the knowledge base does not embed with Azure AI Foundry in this deployment (the operator sets PAD_EMBEDDING_PROVIDER=azure-foundry)", storageif.ErrAzureInvalidDesignation, nil)
	case kb && kind != storageif.AzureKindEmbedding:
		return azureRegErr(http.StatusBadRequest, AzureCodeInvalidDesignation,
			"only an embedding model can be the knowledge base's model", storageif.ErrAzureInvalidDesignation, nil)
	case def && !owner.IsOrg():
		return azureRegErr(http.StatusBadRequest, AzureCodeInvalidDesignation,
			"only an organisation's models have a default for members — your own default for new chats is a setting", storageif.ErrAzureInvalidDesignation, nil)
	case def && kind == storageif.AzureKindEmbedding:
		return azureRegErr(http.StatusBadRequest, AzureCodeInvalidDesignation,
			"an embedding model cannot be the default for members", storageif.ErrAzureInvalidDesignation, nil)
	}
	return nil
}

// reindexRequired is the refusal of a change to the corpus's embedding model.
func reindexRequired() error {
	return azureRegErr(http.StatusConflict, AzureCodeReindexRequired,
		"the knowledge base is indexed with the current knowledge-base model: this change needs a re-index of the knowledge base — confirm to continue", nil, nil)
}

// checkReindex refuses (REINDEX_REQUIRED) a change to owner's
// knowledge-base model while the corpus it embeds is not empty: the
// organisation's own corpus, or — for a user's model, which embeds the
// corpus of every organisation they administer that has no resources of its
// own — any of those. Nothing to refuse where the knowledge base does not
// embed with Azure.
func (s *AzureModelService) checkReindex(ctx context.Context, owner storageif.AzureOwner, scope string) error {
	nonEmpty, err := s.corpusNonEmpty(ctx, owner, scope)
	if err != nil {
		return err
	}
	if nonEmpty {
		return reindexRequired()
	}
	return nil
}

// checkReindexForConnection is checkReindex when resource connID holds the
// knowledge-base model.
func (s *AzureModelService) checkReindexForConnection(ctx context.Context, owner storageif.AzureOwner, scope, connID string) error {
	if !s.knowledgeEmbeddingEnabled {
		return nil
	}
	models, err := s.registry.ListModels(ctx, owner)
	if err != nil {
		return err
	}
	for _, m := range models {
		if m.ConnectionID == connID && m.IsKnowledgeEmbedding {
			return s.checkReindex(ctx, owner, scope)
		}
	}
	return nil
}

// corpusNonEmpty reports whether a corpus owner's knowledge-base model
// embeds holds chunks (see checkReindex).
func (s *AzureModelService) corpusNonEmpty(ctx context.Context, owner storageif.AzureOwner, scope string) (bool, error) {
	if !s.knowledgeEmbeddingEnabled || s.corpus == nil {
		return false, nil
	}
	count := func(orgID string) (bool, error) {
		total, _, err := s.corpus.CountKnowledgeChunks(ctx, orgID)
		if err != nil {
			return false, fmt.Errorf("count the knowledge base's chunks: %w", err)
		}
		return total > 0, nil
	}
	if owner.IsOrg() {
		return count(owner.ID)
	}
	if s.orgs == nil || scope == "" {
		return false, nil
	}
	orgs, err := s.orgs.ListOrgsForUser(ctx, scope)
	if err != nil {
		return false, fmt.Errorf("list the caller's organisations: %w", err)
	}
	for _, o := range orgs {
		if o == nil || !azureOrgAdmin(o, scope) {
			continue
		}
		if has, err := s.OrgHasResources(ctx, o.ID); err != nil {
			return false, err
		} else if has {
			continue // the organisation's own designation embeds its corpus
		}
		if nonEmpty, err := count(o.ID); err != nil || nonEmpty {
			return nonEmpty, err
		}
	}
	return false, nil
}

// azureOrgAdmin reports whether userID administers o (uploads to and
// re-indexes its knowledge base).
func azureOrgAdmin(o *storageif.Organisation, userID string) bool {
	for _, m := range o.Members {
		if m.UserID == userID {
			return m.Role == auth.RoleAdmin
		}
	}
	return false
}

// designationHolder is the model of models holding d ("" when none).
func designationHolder(models []storageif.AzureModelRecord, kb bool) string {
	for _, m := range models {
		if (kb && m.IsKnowledgeEmbedding) || (!kb && m.IsDefault) {
			return m.ID
		}
	}
	return ""
}

// ---- Models ----

// ListModels is GET /models: the scope's models (user scope: the caller's
// own, then the shared models of every organisation where they are an admin
// or member, read-only) in display order, with each one's status and the
// window the server knows for it; in user scope also the server default
// (personal entries, then — when the caller belongs to exactly one such
// organisation — its default for members and its models). Network-free:
// stale project listings refresh in the background.
func (s *AzureModelService) ListModels(ctx context.Context, as AzureScope) (out AzureModelsListing, err error) {
	defer logger.Guard("AzureModels.ListModels", &err)
	owner, err := s.ownerFor(as)
	if err != nil {
		return AzureModelsListing{}, err
	}
	pending := s.prepareOwner(ctx, owner, as.Scope)
	var org *azureOrgInfo
	if owner.IsOrg() {
		org = s.orgInfo(ctx, as.OrgID)
	}
	st, err := s.loadOwner(ctx, owner, as.Scope, org, true)
	if err != nil {
		return AzureModelsListing{}, err
	}
	st.importPending = pending // the server default below never jumps to an organisation
	states := []*azureOwnerState{st}
	var shared []*azureOwnerState
	if !owner.IsOrg() {
		if shared, err = s.sharedStates(ctx, as.Scope); err != nil {
			return AzureModelsListing{}, err
		}
		states = append(states, shared...)
	}
	out.Models = []AzureModelView{}
	for _, ost := range states {
		reported := s.windowsFor(ctx, ost, true)
		for _, m := range ost.models {
			out.Models = append(out.Models, modelView(m, ost.connName(m.ConnectionID), ost.creds[m.ConnectionID], reported[m.ID], ost.org))
		}
		s.refreshListings(ost)
	}
	sortModelViews(out.Models)
	if !owner.IsOrg() {
		var orgState *azureOwnerState
		if len(shared) == 1 {
			orgState = shared[0]
		}
		if id := s.serverDefault(ctx, st, orgState); id != "" {
			out.ServerDefault = ai.RegistryModelID(id)
		}
	}
	return out, nil
}

// GetModel reads one of the scope's own models.
func (s *AzureModelService) GetModel(ctx context.Context, as AzureScope, id string) (out AzureModelView, err error) {
	defer logger.Guard("AzureModels.GetModel", &err)
	owner, err := s.ownerFor(as)
	if err != nil {
		return AzureModelView{}, err
	}
	s.prepareOwner(ctx, owner, as.Scope)
	return s.viewModel(ctx, owner, as.Scope, id)
}

// viewModel builds one of owner's models' view.
func (s *AzureModelService) viewModel(ctx context.Context, owner storageif.AzureOwner, scope, id string) (AzureModelView, error) {
	if !storageif.ValidAzureModelID(id) {
		return AzureModelView{}, storageif.ErrAzureModelNotFound
	}
	var org *azureOrgInfo
	if owner.IsOrg() {
		org = s.orgInfo(ctx, owner.ID)
	}
	st, err := s.loadOwner(ctx, owner, scope, org, false)
	if err != nil {
		return AzureModelView{}, err
	}
	for _, m := range st.models {
		if m.ID != id {
			continue
		}
		cred, err := s.credentialState(owner, scope, m.ConnectionID)
		if err != nil {
			return AzureModelView{}, err
		}
		st.creds[m.ConnectionID] = cred
		reported := s.windowsFor(ctx, st, true)
		return modelView(m, st.connName(m.ConnectionID), cred, reported[m.ID], org), nil
	}
	return AzureModelView{}, storageif.ErrAzureModelNotFound
}

// CreateModel is POST /models: validate, check the resource (an agent needs
// Entra ID and a project endpoint on it: KIND_REQUIRES_PROJECT), the limit,
// the duplicate and the designations, then create it under the owner lock —
// moving a designation it takes from the model that held it.
func (s *AzureModelService) CreateModel(ctx context.Context, as AzureScope, in AzureModelInput) (out AzureModelView, err error) {
	defer logger.Guard("AzureModels.CreateModel", &err)
	owner, err := s.ownerFor(as)
	if err != nil {
		return AzureModelView{}, err
	}
	f := azureModelFields{
		ConnectionID: strings.TrimSpace(in.ConnectionID), Kind: in.Kind, Deployment: in.Deployment,
		DisplayName: in.DisplayName, Description: in.Description,
		ContextWindow: in.ContextWindow, MaxOutputTokens: in.MaxOutputTokens,
	}
	if f.Purpose, f.PurposeCustom, err = normalizeAzurePurpose(in.Purpose, in.PurposeCustom); err != nil {
		return AzureModelView{}, err
	}
	if err := validateModelFields(&f); err != nil {
		return AzureModelView{}, err
	}
	if err := s.designationRules(owner, f.Kind, in.IsKnowledgeEmbedding, in.IsDefault); err != nil {
		return AzureModelView{}, err
	}
	s.prepareOwner(ctx, owner, as.Scope)
	if _, err := s.liveConnection(ctx, owner, f.ConnectionID); err != nil {
		return AzureModelView{}, connectionNotFound(err)
	}
	if err := s.checkKindSupported(owner, as.Scope, f.ConnectionID, f.Kind); err != nil {
		return AzureModelView{}, err
	}
	models, err := s.registry.ListModels(ctx, owner)
	if err != nil {
		return AzureModelView{}, err
	}
	if len(models) >= storageif.AzureMaxModels {
		return AzureModelView{}, tooManyModels()
	}
	if err := duplicateModel(models, "", f); err != nil {
		return AzureModelView{}, err
	}
	if in.IsKnowledgeEmbedding && !in.Force {
		if err := s.checkReindex(ctx, owner, as.Scope); err != nil {
			return AzureModelView{}, err
		}
	}
	id, err := newAzureRegistryID()
	if err != nil {
		return AzureModelView{}, err
	}
	rec := storageif.AzureModelRecord{
		ID: id, ConnectionID: f.ConnectionID, Kind: f.Kind, Deployment: f.Deployment,
		DisplayName: f.DisplayName, Purpose: f.Purpose, PurposeCustom: f.PurposeCustom, Description: f.Description,
		ContextWindow: f.ContextWindow, MaxOutputTokens: f.MaxOutputTokens,
	}
	err = s.registry.WithOwnerLock(ctx, owner, func(lctx context.Context) error {
		counts, err := s.registry.Counts(lctx, owner)
		if err != nil {
			return err
		}
		if counts.Models >= storageif.AzureMaxModels {
			return storageif.ErrAzureTooManyModels
		}
		if _, err := s.registry.CreateModel(lctx, owner, rec); err != nil {
			return err
		}
		if in.IsKnowledgeEmbedding {
			if err := s.registry.SetDesignation(lctx, owner, storageif.AzureDesignationKnowledgeEmbedding, id); err != nil {
				return err
			}
		}
		if in.IsDefault {
			return s.registry.SetDesignation(lctx, owner, storageif.AzureDesignationOrgDefault, id)
		}
		return nil
	})
	if err != nil {
		return AzureModelView{}, s.mapModelWriteErr(ctx, owner, f, err)
	}
	return s.viewModel(ctx, owner, as.Scope, id)
}

// connectionNotFound gives a missing resource its client code (a model's
// resource, named by connectionId).
func connectionNotFound(err error) error {
	if errors.Is(err, storageif.ErrAzureConnectionNotFound) {
		return azureRegErr(http.StatusNotFound, AzureCodeConnectionNotFound, "the resource was not found", err, nil)
	}
	return err
}

// checkKindSupported refuses an agent on a resource whose stored document
// cannot run one (KIND_REQUIRES_PROJECT: an API key, or no project
// endpoint). A resource whose credential is missing or unreadable is not
// judged — its status says so.
func (s *AzureModelService) checkKindSupported(owner storageif.AzureOwner, scope, connID, kind string) error {
	if kind != storageif.AzureKindAgent {
		return nil
	}
	cred, err := s.credentialState(owner, scope, connID)
	if err != nil {
		return err
	}
	if cred.parsed && !cred.cfg.ProjectAccess() {
		return azureRegErr(http.StatusBadRequest, AzureCodeKindRequiresProject,
			"agents need Microsoft Entra ID (or Azure CLI) sign-in and a project endpoint on the resource — edit the resource first, or add the agent to another one", nil, nil)
	}
	return nil
}

// duplicateModel refuses a deployment the resource already registers in
// the same namespace (Claude and chat deployments share one),
// case-insensitively, naming the existing model.
func duplicateModel(models []storageif.AzureModelRecord, skipID string, f azureModelFields) error {
	ns := storageif.AzureModelNamespace(f.Kind)
	for _, m := range models {
		if m.ID == skipID || m.ConnectionID != f.ConnectionID || storageif.AzureModelNamespace(m.Kind) != ns {
			continue
		}
		if strings.EqualFold(m.Deployment, f.Deployment) {
			return azureRegErr(http.StatusConflict, AzureCodeDuplicateModel,
				fmt.Sprintf("this resource already has %q as %q", f.Deployment, m.DisplayName),
				storageif.ErrAzureDuplicateModel, map[string]any{"existingModelId": m.ID})
		}
	}
	return nil
}

// mapModelWriteErr gives the store's model refusals their copy and details.
func (s *AzureModelService) mapModelWriteErr(ctx context.Context, owner storageif.AzureOwner, f azureModelFields, err error) error {
	var re *AzureRegistryError
	switch {
	case errors.As(err, &re):
		return err
	case errors.Is(err, storageif.ErrAzureTooManyModels):
		return tooManyModels()
	case errors.Is(err, storageif.ErrAzureDuplicateModel):
		if models, lerr := s.registry.ListModels(ctx, owner); lerr == nil {
			if derr := duplicateModel(models, "", f); derr != nil {
				return derr
			}
		}
		return azureRegErr(http.StatusConflict, AzureCodeDuplicateModel, "this resource already has a model with this deployment", err, nil)
	case errors.Is(err, storageif.ErrAzureConnectionNotFound):
		return connectionNotFound(err)
	case errors.Is(err, storageif.ErrAzureRegistryConflict), errors.Is(err, storageif.ErrAzureDuplicateDesignation):
		return azureRegErr(http.StatusConflict, AzureCodeConflict, "the model changed since it was loaded — reload it and apply your edit again", err, nil)
	case errors.Is(err, storageif.ErrAzureInvalidDesignation):
		return azureRegErr(http.StatusBadRequest, AzureCodeInvalidDesignation, "the model cannot hold this designation", err, nil)
	}
	return err
}

// UpdateModel is PUT /models/{id}: merge the patch onto the stored model,
// validate the result like a create, then — under the owner lock — clear a
// designation the new kind cannot hold, update the fields and move the
// designations. Changing the knowledge-base model's deployment or resource,
// or making another model the knowledge-base model, on a non-empty corpus is
// 409 REINDEX_REQUIRED unless Force.
func (s *AzureModelService) UpdateModel(ctx context.Context, as AzureScope, id string, in AzureModelPatch) (out AzureModelView, err error) {
	defer logger.Guard("AzureModels.UpdateModel", &err)
	owner, err := s.ownerFor(as)
	if err != nil {
		return AzureModelView{}, err
	}
	expected, err := parseAzureStamp(in.UpdatedAt)
	if err != nil {
		return AzureModelView{}, err
	}
	s.prepareOwner(ctx, owner, as.Scope)
	if !storageif.ValidAzureModelID(id) {
		return AzureModelView{}, storageif.ErrAzureModelNotFound
	}
	cur, err := s.registry.GetModel(ctx, owner, id)
	if err != nil {
		return AzureModelView{}, err
	}
	f, err := mergeModelPatch(cur, in)
	if err != nil {
		return AzureModelView{}, err
	}
	if err := validateModelFields(&f); err != nil {
		return AzureModelView{}, err
	}
	kb := cur.IsKnowledgeEmbedding
	if in.IsKnowledgeEmbedding != nil {
		kb = *in.IsKnowledgeEmbedding
	} else if f.Kind != storageif.AzureKindEmbedding {
		kb = false // a kind that cannot hold it drops it
	}
	def := cur.IsDefault
	if in.IsDefault != nil {
		def = *in.IsDefault
	} else if f.Kind == storageif.AzureKindEmbedding {
		def = false
	}
	if err := s.designationRules(owner, f.Kind, kb && (in.IsKnowledgeEmbedding != nil || !cur.IsKnowledgeEmbedding), def && (in.IsDefault != nil || !cur.IsDefault)); err != nil {
		return AzureModelView{}, err
	}
	if f.ConnectionID != cur.ConnectionID {
		if _, err := s.liveConnection(ctx, owner, f.ConnectionID); err != nil {
			return AzureModelView{}, connectionNotFound(err)
		}
	}
	if f.Kind != cur.Kind || f.ConnectionID != cur.ConnectionID {
		if err := s.checkKindSupported(owner, as.Scope, f.ConnectionID, f.Kind); err != nil {
			return AzureModelView{}, err
		}
	}
	models, err := s.registry.ListModels(ctx, owner)
	if err != nil {
		return AzureModelView{}, err
	}
	if err := duplicateModel(models, id, f); err != nil {
		return AzureModelView{}, err
	}
	corpusChange := (kb && !cur.IsKnowledgeEmbedding) ||
		(kb && cur.IsKnowledgeEmbedding && (!strings.EqualFold(f.Deployment, cur.Deployment) || f.ConnectionID != cur.ConnectionID))
	if corpusChange && !in.Force {
		if err := s.checkReindex(ctx, owner, as.Scope); err != nil {
			return AzureModelView{}, err
		}
	}
	u := storageif.AzureModelUpdate{
		ConnectionID: f.ConnectionID, Kind: f.Kind, Deployment: f.Deployment, DisplayName: f.DisplayName,
		Purpose: f.Purpose, PurposeCustom: f.PurposeCustom, Description: f.Description,
		ContextWindow: f.ContextWindow, MaxOutputTokens: f.MaxOutputTokens, ExpectedUpdatedAt: expected,
	}
	err = s.registry.WithOwnerLock(ctx, owner, func(lctx context.Context) error {
		// Every plan above was made on cur, read before the lock. A
		// designation moved meanwhile (another admin's PUT, which does not
		// move updatedAt) would turn the clears below — which take the
		// designation from whichever model holds it — into a clear of the
		// NEW holder, leaving no knowledge-base model or default for
		// members; and a designation moved onto this model would skip the
		// re-index check. Re-read under the lock: this model's designations
		// must still be the ones planned on (the lock holds every other
		// designation writer off), else the edit is a conflict to reload.
		fresh, err := s.registry.GetModel(lctx, owner, id)
		if err != nil {
			return err
		}
		if fresh.IsKnowledgeEmbedding != cur.IsKnowledgeEmbedding || fresh.IsDefault != cur.IsDefault {
			return storageif.ErrAzureRegistryConflict
		}
		if cur.IsKnowledgeEmbedding && !kb {
			if err := s.registry.SetDesignation(lctx, owner, storageif.AzureDesignationKnowledgeEmbedding, ""); err != nil {
				return err
			}
		}
		if cur.IsDefault && !def {
			if err := s.registry.SetDesignation(lctx, owner, storageif.AzureDesignationOrgDefault, ""); err != nil {
				return err
			}
		}
		if _, err := s.registry.UpdateModel(lctx, owner, id, u); err != nil {
			return err
		}
		if kb && !cur.IsKnowledgeEmbedding {
			if err := s.registry.SetDesignation(lctx, owner, storageif.AzureDesignationKnowledgeEmbedding, id); err != nil {
				return err
			}
		}
		if def && !cur.IsDefault {
			return s.registry.SetDesignation(lctx, owner, storageif.AzureDesignationOrgDefault, id)
		}
		return nil
	})
	if err != nil {
		return AzureModelView{}, s.mapModelWriteErr(ctx, owner, f, err)
	}
	return s.viewModel(ctx, owner, as.Scope, id)
}

// mergeModelPatch applies in onto cur (see AzureModelPatch).
func mergeModelPatch(cur storageif.AzureModelRecord, in AzureModelPatch) (azureModelFields, error) {
	f := azureModelFields{
		ConnectionID: cur.ConnectionID, Kind: cur.Kind, Deployment: cur.Deployment, DisplayName: cur.DisplayName,
		Purpose: cur.Purpose, PurposeCustom: cur.PurposeCustom, Description: cur.Description,
		ContextWindow: in.ContextWindow.apply(cur.ContextWindow), MaxOutputTokens: in.MaxOutputTokens.apply(cur.MaxOutputTokens),
	}
	if in.ConnectionID != nil {
		f.ConnectionID = strings.TrimSpace(*in.ConnectionID)
	}
	if in.Kind != nil {
		f.Kind = *in.Kind
	}
	if in.Deployment != nil {
		f.Deployment = *in.Deployment
	}
	if in.DisplayName != nil {
		f.DisplayName = *in.DisplayName
	}
	if in.Description != nil {
		f.Description = *in.Description
	}
	if strings.TrimSpace(f.Kind) == storageif.AzureKindEmbedding {
		// An embedding model has no window to size and no output to cap:
		// overrides it no longer takes are cleared, not refused.
		if !in.ContextWindow.Set || in.ContextWindow.Null {
			f.ContextWindow = 0
		}
		if !in.MaxOutputTokens.Set || in.MaxOutputTokens.Null {
			f.MaxOutputTokens = 0
		}
	}
	if in.Purpose != nil || in.PurposeCustom != nil {
		// purposeCustom alone means a custom purpose; a preset clears the
		// stored custom text.
		purpose, custom := storageif.AzurePurposeCustom, cur.PurposeCustom
		if in.Purpose != nil {
			purpose = *in.Purpose
			if !strings.EqualFold(strings.TrimSpace(purpose), storageif.AzurePurposeCustom) {
				custom = ""
			}
		}
		if in.PurposeCustom != nil {
			custom = *in.PurposeCustom
		}
		var err error
		if f.Purpose, f.PurposeCustom, err = normalizeAzurePurpose(purpose, custom); err != nil {
			return azureModelFields{}, err
		}
	}
	return f, nil
}

// DeleteModel is DELETE /models/{id}. Deleting the knowledge-base model of a
// non-empty corpus is 409 REINDEX_REQUIRED unless force. Nothing changes in
// Azure.
func (s *AzureModelService) DeleteModel(ctx context.Context, as AzureScope, id string, force bool) (err error) {
	defer logger.Guard("AzureModels.DeleteModel", &err)
	owner, err := s.ownerFor(as)
	if err != nil {
		return err
	}
	s.prepareOwner(ctx, owner, as.Scope)
	if !storageif.ValidAzureModelID(id) {
		return storageif.ErrAzureModelNotFound
	}
	cur, err := s.registry.GetModel(ctx, owner, id)
	if err != nil {
		return err
	}
	if cur.IsKnowledgeEmbedding && !force {
		if err := s.checkReindex(ctx, owner, as.Scope); err != nil {
			return err
		}
	}
	err = s.registry.WithOwnerLock(ctx, owner, func(lctx context.Context) error {
		// The re-index check above was decided on cur: a knowledge-base
		// designation moved onto this model since is a conflict to reload.
		fresh, err := s.registry.GetModel(lctx, owner, id)
		if err != nil {
			return err
		}
		if fresh.IsKnowledgeEmbedding && !cur.IsKnowledgeEmbedding && !force {
			return storageif.ErrAzureRegistryConflict
		}
		_, err = s.registry.DeleteModel(lctx, owner, id)
		return err
	})
	if errors.Is(err, storageif.ErrAzureRegistryConflict) {
		return azureRegErr(http.StatusConflict, AzureCodeConflict, "the model changed since it was loaded — reload it and try again", err, nil)
	}
	return err
}

// recordTest stores a saved resource's or model's last test (best effort:
// the answer is the test's, not the stamp's).
func (s *AzureModelService) recordTest(ctx context.Context, owner storageif.AzureOwner, connID, modelID string, ok bool, latency time.Duration, errText string) {
	res := &storageif.AzureTestResult{OK: ok, At: s.now().UTC(), LatencyMs: int(latency.Milliseconds()), Error: storageif.TruncateAzureTestError(errText)}
	var err error
	if modelID != "" {
		err = s.registry.RecordModelTest(ctx, owner, modelID, res)
	} else {
		err = s.registry.RecordConnectionTest(ctx, owner, connID, res)
	}
	if err != nil {
		logger.Warn("azure registry: recording a test result failed", "owner", owner.String(), "resource", connID, "model", modelID, "error", err)
	}
}
