package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/rag"
	"pad-analyzer/internal/storage"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
	"pad-core/models"
)

// The registry at run time: what a chat turn runs on for a model id
// (ResolveChat), the Azure part of the provider listing (PickerListing), the
// knowledge base's embedder (KnowledgeEmbedder), the one test entry point
// (Test) and discovery (Discover). None of the read paths waits on the
// network: windows and served models come from the process caches, and a
// stale project listing refreshes in the background (refreshListings).

// azureDefaultWindow is the window baki assumes for a model nothing sizes
// (the Azure provider's ContextLimit).
const azureDefaultWindow = 128000

// Background refresh of project listings: at most azureRefreshParallel
// resources at once, each bounded by azureRefreshTimeout.
const (
	azureRefreshParallel = 4
	azureRefreshTimeout  = 45 * time.Second
)

// ---- Chat resolution ----

// AzureChatResolution is what a chat turn runs on for one Azure AI Foundry
// model id (ResolveChat). It travels with the turn — never through the
// request, which is passed by value — to every reader of the model.
type AzureChatResolution struct {
	// ResolvedModel is the provider-level id the turn sends and sizes by:
	// "<deployment>", "claude:<deployment>" or "agent:<name>".
	ResolvedModel string
	// RequestedModel is the id the answer is persisted with: the request's
	// ("m/<entryId>", or a raw pre-registry id), or for "" the server
	// default's "m/<entryId>".
	RequestedModel string
	// ModelLabel is the entry's display name at send time (a raw id resolved
	// through the legacy document: the id itself).
	ModelLabel string
	// Kind is the entry's kind: chat, claude or agent.
	Kind string
	// ConnectionRef names the credential the provider is built from: the
	// caller's own resource (OrgID "") or the organisation's.
	ConnectionRef ai.AzureConnectionRef
	// Entries is the resource's full entry set — ProviderFactory.
	// ForAzureConnection's models (the picker passes the same set, so both
	// share one listing cache).
	Entries []ai.AzureRegisteredModel
	// EntryID is the registry entry ("" for a raw id resolved through the
	// legacy document); Shared marks an organisation's entry.
	EntryID string
	Shared  bool
}

// ResolveChat resolves the model id of a chat turn on provider azure-foundry
// (see AzureChatResolution). Personal entries first, then credOrg's — the
// flow's organisation, passed only when the caller is one of its admins or
// members (the chat service's credentialOrg): no other organisation is ever
// queried, so the answer is no existence or timing oracle.
//
//   - "m/<entryId>": the caller's own entry, else credOrg's;
//   - a raw pre-registry id ("gpt-4.1", "claude:x", "agent:y"; one release):
//     an alias of the caller's own "default" resource; else, when the caller
//     has that resource, the id itself on it (the legacy document took any
//     deployment name); else — only for a caller without it, the member-first
//     rule — an alias of credOrg's "default" resource. A raw id no org entry
//     aliases is refused on the org credential: it keeps the org's curation;
//   - "": the server default (see serverDefault).
//
// Unknown, malformed, foreign, embedding-kind and lost-access ids are all
// ErrModelNotAvailable. A registry or keystore failure is a different error
// (never a fallback to another model or credential) — and so is a legacy
// import still pending (errAzureImportPending): while the caller's legacy
// document is not in the registry, "" and a raw id — which the legacy
// document answers — are refused, never answered from the organisation's
// credential (the member-first rule counts that document as the caller's
// own); an organisation's pending import refuses what only its registry
// could answer. An "m/" id is an explicit pick of an existing entry and
// resolves either way.
func (s *AzureModelService) ResolveChat(ctx context.Context, scope, credOrg, modelID string) (res AzureChatResolution, err error) {
	defer logger.Guard("AzureModels.ResolveChat", &err)
	modelID = strings.TrimSpace(modelID)
	owner := s.userOwner(scope)
	pending := s.prepareOwner(ctx, owner, scope)
	withCreds := modelID == "" // the server default skips resources that cannot run
	personal, err := s.loadOwner(ctx, owner, scope, nil, withCreds)
	if err != nil {
		return AzureChatResolution{}, fmt.Errorf("read the Azure AI Foundry model registry: %w", err)
	}
	personal.importPending = pending
	var orgState *azureOwnerState
	loadOrg := func() error {
		if credOrg == "" || !s.OrgRegistriesAvailable() || orgState != nil {
			return nil
		}
		o := storageif.AzureOrgOwner(credOrg)
		orgPending := !s.ensureLegacyImported(ctx, o, "")
		st, err := s.loadOwner(ctx, o, scope, &azureOrgInfo{id: credOrg}, withCreds)
		if err != nil {
			return fmt.Errorf("read the organisation's Azure AI Foundry model registry: %w", err)
		}
		st.importPending = orgPending
		orgState = st
		return nil
	}

	switch {
	case modelID == "":
		if personal.importPending {
			return AzureChatResolution{}, errAzureImportPending
		}
		if err := loadOrg(); err != nil {
			return AzureChatResolution{}, err
		}
		id := s.serverDefault(ctx, personal, orgState)
		if m, ok := findModel(personal, id); ok {
			return resolveEntry(personal, m, ai.RegistryModelID(m.ID))
		}
		if orgState != nil && orgState.importPending {
			// No personal default, and the organisation's is its legacy
			// document's chat deployment — not in the registry yet.
			return AzureChatResolution{}, errAzureImportPending
		}
		if m, ok := findModel(orgState, id); ok {
			return resolveEntry(orgState, m, ai.RegistryModelID(m.ID))
		}
		return AzureChatResolution{}, ErrModelNotAvailable

	case strings.HasPrefix(modelID, "m/"):
		entryID, ok := ai.ParseRegistryModelID(modelID)
		if !ok {
			return AzureChatResolution{}, ErrModelNotAvailable
		}
		if m, ok := findModel(personal, entryID); ok {
			return resolveEntry(personal, m, modelID)
		}
		if err := loadOrg(); err != nil {
			return AzureChatResolution{}, err
		}
		if orgState != nil {
			if m, ok := findModel(orgState, entryID); ok {
				return resolveEntry(orgState, m, modelID)
			}
		}
		return AzureChatResolution{}, ErrModelNotAvailable
	}

	// A raw (pre-registry) id: the caller's legacy document answers it first,
	// so it cannot be answered while that document is not imported.
	if personal.importPending {
		return AzureChatResolution{}, errAzureImportPending
	}
	if m, ok := aliasMatch(personal, modelID); ok {
		return resolveEntry(personal, m, modelID)
	}
	if _, ok := personal.connection(ai.AzureDefaultConnectionID); ok {
		return resolveLegacyID(personal, modelID)
	}
	if err := loadOrg(); err != nil {
		return AzureChatResolution{}, err
	}
	if orgState != nil {
		if m, ok := aliasMatch(orgState, modelID); ok {
			return resolveEntry(orgState, m, modelID)
		}
		if orgState.importPending {
			// The organisation's legacy document may alias it once imported.
			return AzureChatResolution{}, errAzureImportPending
		}
	}
	return AzureChatResolution{}, ErrModelNotAvailable
}

func findModel(st *azureOwnerState, id string) (storageif.AzureModelRecord, bool) {
	if st == nil || id == "" {
		return storageif.AzureModelRecord{}, false
	}
	for _, m := range st.models {
		if m.ID == id {
			return m, true
		}
	}
	return storageif.AzureModelRecord{}, false
}

// aliasMatch finds the entry of st's "default" resource whose pre-registry
// id is id: exactly, else case-insensitively (Azure deployment names are).
func aliasMatch(st *azureOwnerState, id string) (storageif.AzureModelRecord, bool) {
	if st == nil {
		return storageif.AzureModelRecord{}, false
	}
	var folded *storageif.AzureModelRecord
	for i, m := range st.models {
		if m.ConnectionID != ai.AzureDefaultConnectionID {
			continue
		}
		t := modelTarget(m)
		if t == id {
			return m, true
		}
		if folded == nil && strings.EqualFold(t, id) {
			folded = &st.models[i]
		}
	}
	if folded != nil {
		return *folded, true
	}
	return storageif.AzureModelRecord{}, false
}

// resolveEntry is the resolution of entry m of st (an embedding model is
// never a chat target).
func resolveEntry(st *azureOwnerState, m storageif.AzureModelRecord, requested string) (AzureChatResolution, error) {
	target, ok := ai.AzureModelTarget(m.Kind, m.Deployment)
	if !ok {
		return AzureChatResolution{}, ErrModelNotAvailable
	}
	res := AzureChatResolution{
		ResolvedModel:  target,
		RequestedModel: requested,
		ModelLabel:     m.DisplayName,
		Kind:           m.Kind,
		ConnectionRef:  ai.AzureConnectionRef{ConnID: m.ConnectionID},
		Entries:        st.entriesOf(m.ConnectionID),
		EntryID:        m.ID,
	}
	if st.owner.IsOrg() {
		res.ConnectionRef.OrgID = st.owner.ID
		res.Shared = true
	}
	return res, nil
}

// resolveLegacyID resolves a raw id no entry aliases on the caller's own
// "default" resource, as the legacy document did: any deployment name, a
// "claude:" or "agent:" id. A malformed one is not available.
func resolveLegacyID(st *azureOwnerState, id string) (AzureChatResolution, error) {
	kind := storageif.AzureKindChat
	name := id
	switch {
	case strings.HasPrefix(id, "agent:"):
		kind, name = storageif.AzureKindAgent, strings.TrimPrefix(id, "agent:")
		if !ai.ValidAzureAgentName(name) {
			return AzureChatResolution{}, ErrModelNotAvailable
		}
	case strings.HasPrefix(id, "claude:"):
		kind, name = storageif.AzureKindClaude, strings.TrimPrefix(id, "claude:")
		if !ai.ValidAzureDeploymentName(name) {
			return AzureChatResolution{}, ErrModelNotAvailable
		}
	default:
		if !ai.ValidAzureDeploymentName(name) {
			return AzureChatResolution{}, ErrModelNotAvailable
		}
	}
	return AzureChatResolution{
		ResolvedModel:  id,
		RequestedModel: id,
		ModelLabel:     id,
		Kind:           kind,
		ConnectionRef:  ai.AzureConnectionRef{ConnID: ai.AzureDefaultConnectionID},
		Entries:        st.entriesOf(ai.AzureDefaultConnectionID),
	}, nil
}

// ---- The server default ----

// legacyDefaultTarget is the model the legacy document offered as its
// default: its chat deployment (else the first additional one, else the
// first Claude deployment). The document keeps those fields unchanged
// (rollback), so this is the entry the import made of it.
func legacyDefaultTarget(cfg ai.AzureFoundryConfig) string {
	switch {
	case cfg.ChatDeployment != "":
		return cfg.ChatDeployment
	case len(cfg.Deployments) > 0:
		return cfg.Deployments[0]
	case len(cfg.ClaudeDeployments) > 0:
		return "claude:" + cfg.ClaudeDeployments[0]
	}
	return ""
}

// serverDefault is the entry a new chat starts on when the user saved no
// default of their own — member-first, like the credential rule:
//  1. the personal entry imported from the legacy document's chat
//     deployment;
//  2. the first personal chat entry (chat and Claude before agents);
//  3. the organisation's "Default for members";
//  4. the first organisation chat entry.
//
// Only usable entries count (their resource ready, an agent runnable); ""
// when there is none. org may be nil. The states must carry credentials.
// While either owner's legacy import is pending the organisation is not
// consulted: the caller's pending legacy document is a default of their
// own (member-first), and the organisation's pending one is its default
// for members — neither is in the registry yet.
func (s *AzureModelService) serverDefault(_ context.Context, personal, org *azureOwnerState) string {
	usable := func(st *azureOwnerState, m storageif.AzureModelRecord) bool {
		if m.Kind == storageif.AzureKindEmbedding {
			return false
		}
		status, _ := modelStatus(m, st.creds[m.ConnectionID])
		return status == AzureStatusReady
	}
	first := func(st *azureOwnerState) string {
		if st == nil {
			return ""
		}
		var cands []storageif.AzureModelRecord
		for _, m := range st.models {
			if usable(st, m) {
				cands = append(cands, m)
			}
		}
		sort.SliceStable(cands, func(i, j int) bool {
			agentI, agentJ := cands[i].Kind == storageif.AzureKindAgent, cands[j].Kind == storageif.AzureKindAgent
			if agentI != agentJ {
				return !agentI
			}
			a, b := cands[i], cands[j]
			return azureEntryLess(a.Purpose, a.PurposeCustom, false, a.DisplayName, a.ID, b.Purpose, b.PurposeCustom, false, b.DisplayName, b.ID)
		})
		if len(cands) == 0 {
			return ""
		}
		return cands[0].ID
	}
	if personal != nil {
		if cred := personal.creds[ai.AzureDefaultConnectionID]; cred.parsed {
			if target := legacyDefaultTarget(cred.cfg); target != "" {
				if m, ok := aliasMatch(personal, target); ok && usable(personal, m) {
					return m.ID
				}
			}
		}
		if id := first(personal); id != "" {
			return id
		}
		if personal.importPending {
			return ""
		}
	}
	if org != nil && !org.importPending {
		for _, m := range org.models {
			if m.IsDefault && usable(org, m) {
				return m.ID
			}
		}
		return first(org)
	}
	return ""
}

// ---- The picker ----

// AzurePickerListing is the Azure AI Foundry part of the provider listing
// (models.ProviderInfo), built from the registry: every chat, Claude and
// agent entry is listed whatever a live listing says — live data only
// decorates. Network-free.
type AzurePickerListing struct {
	// Models are the caller's own entries and orgID's shared ones in display
	// order (purpose, then personal before shared, then name). Entries whose
	// resource cannot run them stay listed with their status.
	Models []models.ModelDetail
	// DefaultModel is the server default's "m/<id>" ("" when there is none).
	DefaultModel string
	// Configured: the caller has a resource of their own. Shared: they have
	// none, and orgID has one.
	Configured bool
	Shared     bool
	// SuggestDiscovery offers the "your project has models you haven't
	// added" prompt: the caller's migrated resource ("default") can list its
	// project (Entra ID or the Azure CLI with a project endpoint), its prompt
	// was not dismissed, and the registry takes writes.
	SuggestDiscovery bool
}

// PickerListing builds the Azure part of the provider listing for the
// caller with key scope scope; orgID is the open flow's organisation, whose
// shared entries are listed — the caller's role in it (admin or member) is
// the handler's check. Stale project listings refresh in the background (at
// most four resources at once).
//
// A legacy import still pending never fails the listing, nor moves the
// caller onto the organisation: while the caller's own legacy document is
// not in the registry it keeps them configured and counts as their
// "default" resource for the member-first rule — no organisation alias is
// listed (a client would re-pin the caller's old raw-id threads to the
// organisation's entries) and the server default is never the
// organisation's; while the organisation's is pending it counts as a
// resource (shared) but gives no default.
func (s *AzureModelService) PickerListing(ctx context.Context, scope, orgID string) (out AzurePickerListing, err error) {
	defer logger.Guard("AzureModels.PickerListing", &err)
	owner := s.userOwner(scope)
	pending := s.prepareOwner(ctx, owner, scope)
	personal, err := s.loadOwner(ctx, owner, scope, nil, true)
	if err != nil {
		return AzurePickerListing{}, err
	}
	personal.importPending = pending
	var org *azureOwnerState
	if orgID != "" && s.OrgRegistriesAvailable() {
		o := storageif.AzureOrgOwner(orgID)
		orgPending := !s.ensureLegacyImported(ctx, o, "")
		if org, err = s.loadOwner(ctx, o, scope, s.orgInfo(ctx, orgID), true); err != nil {
			return AzurePickerListing{}, err
		}
		org.importPending = orgPending
	}
	out.Configured = len(personal.conns) > 0 || personal.importPending
	if !out.Configured {
		if has, _ := s.secrets.Has(scope, ai.AzureFoundryProviderID); has {
			out.Configured = true // the legacy document, while its import cannot run
		}
	}
	out.Shared = !out.Configured && org != nil && (len(org.conns) > 0 || org.importPending)
	if c, ok := personal.connection(ai.AzureDefaultConnectionID); ok {
		cred := personal.creds[c.ID]
		out.SuggestDiscovery = cred.parsed && cred.cfg.ProjectAccess() && c.DiscoveryDismissedAt == nil &&
			s.registry.State(ctx).State != storageif.AzureRegistryReadOnly
	}
	defaultID := s.serverDefault(ctx, personal, org)
	if defaultID != "" {
		out.DefaultModel = ai.RegistryModelID(defaultID)
	}
	_, hasPersonalDefault := personal.connection(ai.AzureDefaultConnectionID)
	hasPersonalDefault = hasPersonalDefault || personal.importPending
	out.Models = []models.ModelDetail{}
	for _, st := range []*azureOwnerState{personal, org} {
		if st == nil {
			continue
		}
		windows := s.windowsFor(ctx, st, false)
		for _, m := range st.models {
			if m.Kind == storageif.AzureKindEmbedding {
				continue
			}
			status, reason := modelStatus(m, st.creds[m.ConnectionID])
			d := models.ModelDetail{
				ID:             ai.RegistryModelID(m.ID),
				DisplayName:    m.DisplayName,
				ContextLimit:   windows[m.ID],
				SupportsTools:  true,
				Kind:           m.Kind,
				Purpose:        m.Purpose,
				PurposeCustom:  m.PurposeCustom,
				Description:    m.Description,
				Shared:         st.owner.IsOrg(),
				ConnectionID:   m.ConnectionID,
				ConnectionName: st.connName(m.ConnectionID),
				Status:         status,
				StatusReason:   reason,
				IsDefault:      m.ID == defaultID,
				IsOrgDefault:   st.owner.IsOrg() && m.IsDefault,
			}
			if d.ContextLimit <= 0 {
				d.ContextLimit = azureDefaultWindow
			}
			if st.org != nil {
				d.OrgID, d.OrgName = st.org.id, st.org.name
			}
			// Aliases (the pre-registry ids) only on the "default" resource's
			// entries — and an organisation's only for a member without a
			// "default" resource of their own (member-first).
			if m.ConnectionID == ai.AzureDefaultConnectionID && (!st.owner.IsOrg() || !hasPersonalDefault) {
				d.Aliases = []string{modelTarget(m)}
			}
			out.Models = append(out.Models, d)
		}
		s.refreshListings(st)
	}
	sort.SliceStable(out.Models, func(i, j int) bool {
		a, b := out.Models[i], out.Models[j]
		return azureEntryLess(a.Purpose, a.PurposeCustom, a.Shared, a.DisplayName, a.ID, b.Purpose, b.PurposeCustom, b.Shared, b.DisplayName, b.ID)
	})
	return out, nil
}

// windowsFor sizes st's chat, Claude and agent entries without the network:
// from the provider each ready resource runs on (its listing is the
// entries plus the cached project listing, decorated by the served-model
// memo and clamped by learned windows). reported leaves the users'
// overrides out — the window the server knows by itself, 0 when nothing
// sizes the model (baki then assumes azureDefaultWindow); otherwise the
// effective window, override included. Entries of a resource that cannot
// run are sized by their override, else the catalogue.
func (s *AzureModelService) windowsFor(ctx context.Context, st *azureOwnerState, reported bool) map[string]int {
	out := map[string]int{}
	catalogue := azureCatalogueWindows(ctx)
	for _, c := range st.conns {
		entries := st.entriesOf(c.ID)
		listed := map[string]ai.ModelInfo{}
		if cred := st.creds[c.ID]; cred.ready() && s.factory != nil && hasChatEntry(entries) {
			use := entries
			if reported {
				use = make([]ai.AzureRegisteredModel, len(entries))
				for i, e := range entries {
					e.ContextWindow, e.MaxOutputTokens = 0, 0
					use[i] = e
				}
			}
			ref := ai.AzureConnectionRef{ConnID: c.ID}
			if st.owner.IsOrg() {
				ref.OrgID = st.owner.ID
			}
			if p, err := s.factory.ForAzureConnection(st.scope, ref, use); err == nil {
				if ms, err := p.Models(ctx); err == nil {
					for _, m := range ms {
						listed[m.ID] = m
					}
				}
			}
		}
		for _, m := range st.models {
			if m.ConnectionID != c.ID || m.Kind == storageif.AzureKindEmbedding {
				continue
			}
			w := 0
			if l, ok := listed[modelTarget(m)]; ok && l.ContextLimit > 0 {
				w = l.ContextLimit
				if reported && w == azureDefaultWindow && l.ServedModel == "" && catalogue[strings.ToLower(m.Deployment)] == 0 {
					w = 0 // the provider-wide assumption, not a known window
				}
			} else {
				if !reported && m.ContextWindow > 0 {
					w = m.ContextWindow
				} else if m.Kind != storageif.AzureKindAgent {
					w = catalogue[strings.ToLower(m.Deployment)]
				}
			}
			out[m.ID] = w
		}
	}
	return out
}

// hasChatEntry reports an entry the chat path can run (not an embedding).
func hasChatEntry(entries []ai.AzureRegisteredModel) bool {
	for _, e := range entries {
		if e.Kind != storageif.AzureKindEmbedding {
			return true
		}
	}
	return false
}

// azureCatalogueWindows is the Azure reference catalogue's windows by
// lower-cased model id (a deployment named after its model is sized by it).
func azureCatalogueWindows(ctx context.Context) map[string]int {
	out := map[string]int{}
	p := ai.GetMetadataProvider(ai.AzureFoundryProviderID)
	if p == nil {
		return out
	}
	ms, err := p.Models(ctx)
	if err != nil {
		return out
	}
	for _, m := range ms {
		if m.ContextLimit > 0 {
			out[strings.ToLower(m.ID)] = m.ContextLimit
		}
	}
	return out
}

// ---- Background listing refresh ----

// azureRefreshQueueMax bounds the refreshes waiting for a worker; past it a
// read's refresh is dropped (the next read asks again).
const azureRefreshQueueMax = 64

// azureListingRefresher refreshes stale project listings off the read
// paths: one refresh per resource at a time (running or queued — a
// singleflight), at most azureRefreshParallel running at once. A refresh
// that finds every worker busy waits in a bounded queue the workers drain,
// so a resource beyond the first four is refreshed too, never starved by
// the order the reads list resources in.
//
// A job names its resource, not its credential: it reads the credential
// when it runs and is skipped when the credential is gone or is no longer
// the one the read saw (a job queued before a delete, a rotation or a
// lender's disable must not sign in with the old document — that would put
// back the Entra ID token source ForgetAzureFoundryCredential just dropped,
// and list the project with a principal that was removed or demoted).
type azureListingRefresher struct {
	refresh  func(ctx context.Context, raw string, models []ai.AzureRegisteredModel) error
	mu       sync.Mutex
	running  int
	inflight map[string]bool
	queue    []azureRefreshJob
	wg       sync.WaitGroup
}

// azureRefreshJob is one resource's refresh: owner's resource connID (its
// personal credential under scope), the fingerprint of the credential the
// triggering read saw, and the resource's entries.
type azureRefreshJob struct {
	key         string
	owner       storageif.AzureOwner
	scope       string
	connID      string
	fingerprint string
	entries     []ai.AzureRegisteredModel
	// read reads the resource's credential as stored when the job runs.
	read func(owner storageif.AzureOwner, scope, connID string) (string, error)
}

func newAzureListingRefresher(refresh func(ctx context.Context, raw string, models []ai.AzureRegisteredModel) error) *azureListingRefresher {
	return &azureListingRefresher{refresh: refresh, inflight: map[string]bool{}}
}

// trigger schedules job unless its resource's refresh is running or
// queued. Never blocks.
func (r *azureListingRefresher) trigger(job azureRefreshJob) {
	r.mu.Lock()
	defer r.mu.Unlock()
	key := job.key
	if r.inflight[key] {
		return
	}
	if r.running >= azureRefreshParallel {
		if len(r.queue) >= azureRefreshQueueMax {
			return
		}
		r.inflight[key] = true
		r.queue = append(r.queue, job)
		return
	}
	r.inflight[key] = true
	r.running++
	r.wg.Add(1)
	go r.work(job)
}

// work runs job, then the queued jobs, until the queue is empty.
func (r *azureListingRefresher) work(job azureRefreshJob) {
	defer r.wg.Done()
	for {
		r.run(job)
		r.mu.Lock()
		delete(r.inflight, job.key)
		if len(r.queue) == 0 {
			r.running--
			r.mu.Unlock()
			return
		}
		job = r.queue[0]
		r.queue = r.queue[1:]
		r.mu.Unlock()
	}
}

// run refreshes one listing, bounded by azureRefreshTimeout, on the
// resource's credential as it is stored NOW: skipped when it is gone or is
// not the one the triggering read saw (see azureListingRefresher). A panic
// is logged, never fatal (nothing above a detached goroutine recovers it).
func (r *azureListingRefresher) run(job azureRefreshJob) {
	defer func() {
		if rec := recover(); rec != nil {
			logger.Warn("azure registry: a background listing refresh panicked", "panic", rec)
		}
	}()
	raw, ok := job.current()
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), azureRefreshTimeout)
	defer cancel()
	if err := r.refresh(ctx, raw, job.entries); err != nil {
		logger.Warn("azure registry: refreshing a project listing failed", "error", err)
	}
}

// current reads the job's credential and reports whether it is still the
// one the job was queued for (same fingerprint).
func (j azureRefreshJob) current() (string, bool) {
	if j.read == nil {
		return "", false
	}
	raw, err := j.read(j.owner, j.scope, j.connID)
	if err != nil {
		logger.Warn("azure registry: a background listing refresh could not read its credential; skipped", "owner", j.owner.String(), "resource", j.connID, "error", err)
		return "", false
	}
	if strings.TrimSpace(raw) == "" {
		return "", false // deleted (or disabled) since the read
	}
	cfg, err := ai.ParseAzureFoundryConfig(raw)
	if err != nil || cfg.Fingerprint() != j.fingerprint {
		return "", false // replaced since the read: the next read refreshes it
	}
	return raw, true
}

// refreshListings triggers a background refresh of st's resources that
// list a project: ready, with Entra ID (or the Azure CLI) and a project
// endpoint, and at least one chat entry. A listing still live costs nothing
// (ai.RefreshAzureFoundryListing returns at once).
func (s *AzureModelService) refreshListings(st *azureOwnerState) {
	if s.refresher == nil {
		return
	}
	for _, c := range st.conns {
		cred := st.creds[c.ID]
		entries := st.entriesOf(c.ID)
		if !cred.ready() || !cred.cfg.ProjectAccess() || !hasChatEntry(entries) {
			continue
		}
		s.refresher.trigger(azureRefreshJob{
			key: st.owner.String() + "\x00" + c.ID, owner: st.owner, scope: st.scope, connID: c.ID,
			fingerprint: cred.cfg.Fingerprint(), entries: entries, read: s.readCredential,
		})
	}
}

// ---- The knowledge base's embedder ----

// azureEmbedderError is a knowledge-base embedder refusal with its own copy,
// matching sentinel (ai.ErrOrgEmbeddingDeploymentMissing: the org admin's
// to fix, no personal fallback; ai.ErrEmbeddingDeploymentMissing: the
// caller's) and optional cause.
type azureEmbedderError struct {
	msg      string
	sentinel error
	cause    error
}

func (e *azureEmbedderError) Error() string { return e.msg }

func (e *azureEmbedderError) Unwrap() []error {
	if e.cause == nil {
		return []error{e.sentinel}
	}
	return []error{e.sentinel, e.cause}
}

// KnowledgeEmbedder resolves the knowledge base's embedder from the
// registry (rag.WithAzureEmbedder): for an organisation's corpus the
// organisation's "Use for knowledge base" model whenever the organisation
// holds resources — none designated is an ai.ErrOrgEmbeddingDeploymentMissing
// refusal (EMBEDDING_ORG_NOT_CONFIGURED), never a member's personal model:
// one corpus, one model; otherwise the caller's own designated model (an
// ai.ErrEmbeddingDeploymentMissing refusal when there is none, which lets
// the knowledge base look further, as before). The provider is built by
// ProviderFactory.ForAzureConnectionEmbedding on exactly that resource's
// credential.
//
// An organisation whose legacy import is pending counts as holding
// resources (its legacy document is one), and a caller whose own import is
// pending has no usable "none designated" (the legacy embedding deployment
// may be the designation): without a designated entry either is a
// transient refusal, never a fallback to another model. So is a registry or
// key-store failure — logged, and answered with rag.ErrEmbeddingSetupUnavailable
// (503 with masked copy), never with the store's own text.
func (s *AzureModelService) KnowledgeEmbedder(ctx context.Context, scope, orgID string) (p ai.Provider, err error) {
	defer logger.Guard("AzureModels.KnowledgeEmbedder", &err)
	if s.factory == nil {
		return nil, fmt.Errorf("%s: %w", ai.AzureFoundryProviderID, ai.ErrKeyNotConfigured)
	}
	if orgID != "" && s.OrgRegistriesAvailable() {
		owner := storageif.AzureOrgOwner(orgID)
		pending := !s.ensureLegacyImported(ctx, owner, "")
		st, err := s.loadOwner(ctx, owner, scope, nil, false)
		if err != nil {
			return nil, knowledgeEmbedderUnavailable(owner, fmt.Errorf("read the organisation's Azure AI Foundry model registry: %w", err))
		}
		if len(st.conns) > 0 || pending {
			m, ok := knowledgeEntry(st)
			switch {
			case !ok && pending:
				return nil, knowledgeEmbedderUnavailable(owner, errAzureImportPending)
			case !ok:
				return nil, &azureEmbedderError{sentinel: ai.ErrOrgEmbeddingDeploymentMissing,
					msg: "the organisation has Azure AI Foundry resources but none of its embedding models is used for the knowledge base — ask an org admin to mark one \"Use for knowledge base\" (Settings → Organizations)"}
			}
			p, err := s.factory.ForAzureConnectionEmbedding(scope, ai.AzureConnectionRef{OrgID: orgID, ConnID: m.ConnectionID}, registeredModel(m))
			return knowledgeEmbedderResult(owner, m, p, err)
		}
	}
	owner := s.userOwner(scope)
	pending := s.prepareOwner(ctx, owner, scope)
	st, err := s.loadOwner(ctx, owner, scope, nil, false)
	if err != nil {
		return nil, knowledgeEmbedderUnavailable(owner, fmt.Errorf("read the Azure AI Foundry model registry: %w", err))
	}
	m, ok := knowledgeEntry(st)
	switch {
	case !ok && pending:
		return nil, knowledgeEmbedderUnavailable(owner, errAzureImportPending)
	case !ok:
		return nil, &azureEmbedderError{sentinel: ai.ErrEmbeddingDeploymentMissing,
			msg: "no Azure AI Foundry embedding model is used for the knowledge base — mark one \"Use for knowledge base\" under Settings → AI Accounts → Azure AI Foundry"}
	}
	p, err = s.factory.ForAzureConnectionEmbedding(scope, ai.AzureConnectionRef{ConnID: m.ConnectionID}, registeredModel(m))
	return knowledgeEmbedderResult(owner, m, p, err)
}

// knowledgeEmbedderResult classifies the factory's answer for owner's
// knowledge-base model m. The configuration gaps keep their sentinels and
// copy — no credential (an organisation's: the org admin's to fix, never a
// personal fallback; a caller's own: the knowledge base looks further, as
// before), settings that no longer validate (ai.ErrProviderConfigInvalid),
// credentials the server can no longer decrypt (enter them again). Anything
// else is a key-store failure: knowledgeEmbedderUnavailable.
func knowledgeEmbedderResult(owner storageif.AzureOwner, m storageif.AzureModelRecord, p ai.Provider, err error) (ai.Provider, error) {
	switch {
	case err == nil:
		return p, nil
	case errors.Is(err, ai.ErrKeyNotConfigured) && owner.IsOrg():
		return nil, &azureEmbedderError{sentinel: ai.ErrOrgEmbeddingDeploymentMissing, cause: err,
			msg: fmt.Sprintf("the organisation's knowledge-base model %q cannot be used: its resource has no credentials — ask an org admin to enter them again", m.DisplayName)}
	case errors.Is(err, storage.ErrSecretDecryptFailed) && owner.IsOrg():
		return nil, &azureEmbedderError{sentinel: ai.ErrOrgEmbeddingDeploymentMissing,
			msg: fmt.Sprintf("the organisation's knowledge-base model %q cannot be used: its resource's stored credentials could not be read — ask an org admin to enter them again", m.DisplayName)}
	case errors.Is(err, storage.ErrSecretDecryptFailed):
		return nil, &azureEmbedderError{sentinel: ai.ErrProviderConfigInvalid,
			msg: fmt.Sprintf("the knowledge base's model %q cannot be used: its resource's stored credentials could not be read — enter them again under Settings → AI Accounts → Azure AI Foundry", m.DisplayName)}
	case errors.Is(err, ai.ErrKeyNotConfigured), errors.Is(err, ai.ErrProviderConfigInvalid),
		errors.Is(err, ai.ErrEmbeddingDeploymentMissing), errors.Is(err, ai.ErrOrgEmbeddingDeploymentMissing):
		return nil, err
	}
	return nil, knowledgeEmbedderUnavailable(owner, err)
}

// knowledgeEmbedderUnavailable logs why the knowledge base's embedder could
// not be decided for owner — a registry or key-store failure, a legacy
// import pending — and returns rag.ErrEmbeddingSetupUnavailable: a transient
// refusal the knowledge base never answers with another model, whose copy
// (unlike the cause's: a driver's error names database hosts and roles) the
// client may see. The cause is logged only, never wrapped: nothing in it
// may turn the refusal into a fallback.
func knowledgeEmbedderUnavailable(owner storageif.AzureOwner, cause error) error {
	logger.Warn("azure registry: the knowledge base's embedder could not be resolved", "owner", owner.String(), "error", cause)
	return &azureEmbedderError{sentinel: rag.ErrEmbeddingSetupUnavailable, msg: rag.ErrEmbeddingSetupUnavailable.Error()}
}

// knowledgeEntry is st's knowledge-base model.
func knowledgeEntry(st *azureOwnerState) (storageif.AzureModelRecord, bool) {
	for _, m := range st.models {
		if m.IsKnowledgeEmbedding && m.Kind == storageif.AzureKindEmbedding {
			return m, true
		}
	}
	return storageif.AzureModelRecord{}, false
}

// ---- Tests ----

// AzureTestModelRef names the model of a test: a saved one by its entry id
// ([a-z0-9]{10}, not "m/…"), or a draft {kind, deployment} on the test's
// resource.
type AzureTestModelRef struct {
	ID         string `json:"id,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Deployment string `json:"deployment,omitempty"`
}

// AzureTestInput is POST /test: {connectionId} tests a saved resource,
// {config} a draft one — with connectionId too, the stored secret carries
// over by the host rules — and model adds a model test on that resource.
type AzureTestInput struct {
	ConnectionID string                      `json:"connectionId,omitempty"`
	Config       *AzureConnectionConfigInput `json:"config,omitempty"`
	Model        *AzureTestModelRef          `json:"model,omitempty"`
}

// AzureTestResult is POST /test's answer for every test outcome (a 4xx is
// only for an invalid request). Code classifies a failure.
type AzureTestResult struct {
	OK        bool   `json:"ok"`
	LatencyMs int    `json:"latencyMs"`
	Error     string `json:"error,omitempty"`
	Code      string `json:"code,omitempty"`
}

// Test is the one test entry point: a saved or draft resource, with or
// without a saved or draft model on it. A resource test signs in and probes
// the endpoint (and the project) — no model call, no cost; a chat, Claude or
// embedding model test sends one tiny billed request; an agent test only
// looks the agent up — it is never run. A draft goes through the carry-
// secret rules, then validation, before any network call (an organisation's
// draft never signs in with the Azure CLI); probes run isolated, with a
// throwaway token source, and record no usage. A saved target records its
// last test. Errors are only for invalid requests (a draft that does not
// validate, CONNECTION_NOT_FOUND, MODEL_NOT_FOUND).
func (s *AzureModelService) Test(ctx context.Context, as AzureScope, in AzureTestInput) (out AzureTestResult, err error) {
	defer logger.Guard("AzureModels.Test", &err)
	owner, err := s.ownerFor(as)
	if err != nil {
		return AzureTestResult{}, err
	}
	s.prepareOwner(ctx, owner, as.Scope)
	connID := strings.TrimSpace(in.ConnectionID)

	var savedModel *storageif.AzureModelRecord
	var draftModel *ai.AzureRegisteredModel
	if in.Model != nil {
		if id := strings.TrimSpace(in.Model.ID); id != "" {
			m, err := s.testModelRecord(ctx, owner, id, connID)
			if err != nil {
				return AzureTestResult{}, err
			}
			connID = m.ConnectionID
			savedModel = &m
		} else {
			d := ai.AzureRegisteredModel{Kind: strings.TrimSpace(in.Model.Kind), Deployment: strings.TrimSpace(in.Model.Deployment)}
			if err := ai.ValidateAzureRegisteredModel(d); err != nil {
				return AzureTestResult{}, err
			}
			draftModel = &d
		}
	}

	var raw string
	if in.Config != nil {
		cfg := in.Config.document()
		if connID != "" {
			if _, err := s.liveConnection(ctx, owner, connID); err != nil {
				return AzureTestResult{}, connectionNotFound(err)
			}
			prev, hadPrev := storedAzureConfig(func() (string, error) { return s.readCredential(owner, as.Scope, connID) })
			validate := func(c ai.AzureFoundryConfig) error { return validateConnectionDocument(c, connID, owner.IsOrg()) }
			if refusal := carryAzureSecret(&cfg, prev, hadPrev); refusal != nil {
				return AzureTestResult{}, withAuthField(cfg, saveCarryErrorWith(cfg, refusal, validate))
			}
		}
		if err := validateConnectionDocument(cfg, connID, owner.IsOrg()); err != nil {
			return AzureTestResult{}, err
		}
		if raw, err = cfg.Encode(); err != nil {
			return AzureTestResult{}, err
		}
	} else {
		if connID == "" {
			return AzureTestResult{}, &ai.AzureConfigError{Code: ai.AzureCfgErrInvalidConfig, Message: "name the resource to test (connectionId) or send its settings (config)"}
		}
		if _, err := s.liveConnection(ctx, owner, connID); err != nil {
			return AzureTestResult{}, connectionNotFound(err)
		}
		cred, err := s.credentialState(owner, as.Scope, connID)
		if err != nil {
			return AzureTestResult{}, err
		}
		if !cred.ready() {
			out = AzureTestResult{OK: false, Error: cred.reason, Code: strings.ToUpper(cred.status)}
			if draftModel == nil {
				s.recordTest(ctx, owner, connID, savedModelID(savedModel, nil), false, 0, out.Error)
			}
			return out, nil
		}
		raw = cred.raw
	}

	start := s.now()
	var perr error
	switch {
	case savedModel != nil:
		perr = s.probeModel(ctx, raw, registeredModel(*savedModel))
	case draftModel != nil:
		perr = s.probeModel(ctx, raw, *draftModel)
	default:
		perr = s.probeResource(ctx, raw)
	}
	latency := s.now().Sub(start)
	out = AzureTestResult{OK: perr == nil, LatencyMs: int(latency.Milliseconds())}
	if perr != nil {
		out.Error = connectionTestError(ai.AzureFoundryProviderID, "", perr)
		out.Code = azureFailureCode(perr)
	}
	if in.Config == nil && draftModel == nil {
		// A saved target — the resource, or a saved model on it.
		s.recordTest(ctx, owner, connID, savedModelID(savedModel, nil), out.OK, latency, out.Error)
	}
	return out, nil
}

// savedModelID is the entry id a test result is recorded on ("" = the
// resource's; nothing is recorded for a draft model).
func savedModelID(saved *storageif.AzureModelRecord, draft *ai.AzureRegisteredModel) string {
	if saved != nil && draft == nil {
		return saved.ID
	}
	return ""
}

// testModelRecord reads the saved model a test names (MODEL_NOT_FOUND when
// it is not the scope's, or not on connID when one was named).
func (s *AzureModelService) testModelRecord(ctx context.Context, owner storageif.AzureOwner, id, connID string) (storageif.AzureModelRecord, error) {
	notFound := azureRegErr(http.StatusNotFound, AzureCodeModelNotFound, "the model was not found", storageif.ErrAzureModelNotFound, nil)
	if !storageif.ValidAzureModelID(id) {
		return storageif.AzureModelRecord{}, notFound
	}
	m, err := s.registry.GetModel(ctx, owner, id)
	if errors.Is(err, storageif.ErrAzureModelNotFound) || (err == nil && connID != "" && m.ConnectionID != connID) {
		return storageif.AzureModelRecord{}, notFound
	}
	if err != nil {
		return storageif.AzureModelRecord{}, err
	}
	if _, err := s.liveConnection(ctx, owner, m.ConnectionID); err != nil {
		if errors.Is(err, storageif.ErrAzureConnectionNotFound) {
			return storageif.AzureModelRecord{}, notFound
		}
		return storageif.AzureModelRecord{}, err
	}
	return m, nil
}

// azureFailureCode classifies a test's or discovery's failure for clients.
func azureFailureCode(err error) string {
	var ce *ai.AzureConfigError
	switch {
	case errors.As(err, &ce):
		return ce.Code
	case errors.Is(err, ai.ErrAzureAgentNotFound):
		return "AGENT_NOT_FOUND"
	case errors.Is(err, ai.ErrAzureAgentNotRunnable):
		return "AGENT_NOT_RUNNABLE"
	case errors.Is(err, ai.ErrAzureProjectNotFound):
		return "PROJECT_NOT_FOUND"
	case errors.Is(err, ai.ErrAzureEndpointNotFound):
		return "ENDPOINT_NOT_FOUND"
	case errors.Is(err, ai.ErrEntraCredentialsRejected):
		return "ENTRA_CREDENTIALS_REJECTED"
	case errors.Is(err, ai.ErrEntraTokenUnavailable):
		return "ENTRA_TOKEN_UNAVAILABLE"
	case errors.Is(err, ai.ErrProviderPermissionDenied): // before ErrApiKeyInvalid, which it also matches
		return "PERMISSION_DENIED"
	case errors.Is(err, ai.ErrEntraTokenRejected):
		return "ENTRA_TOKEN_REJECTED"
	case errors.Is(err, ai.ErrClaudeEntraOnly):
		return "ENTRA_ONLY"
	case errors.Is(err, ai.ErrApiKeyInvalid):
		return "INVALID_API_KEY"
	case errors.Is(err, ai.ErrRateLimited):
		return "RATE_LIMITED"
	case errors.Is(err, ai.ErrContentFiltered):
		return "CONTENT_FILTERED"
	case errors.Is(err, context.DeadlineExceeded):
		return "TIMEOUT"
	case errors.Is(err, ai.ErrProviderConfigInvalid):
		return ai.AzureCfgErrInvalidConfig
	case errors.Is(err, ai.ErrProviderDown), errors.Is(err, ai.ErrCircuitOpen):
		return "PROVIDER_UNAVAILABLE"
	}
	if _, ok := egress.DescribeFailure(err); ok {
		return "CONNECTION_FAILED"
	}
	return "TEST_FAILED"
}

// ---- Discovery ----

// AzureDiscoveryItemView is one deployment or agent discovery found.
type AzureDiscoveryItemView struct {
	Name string `json:"name"`
	// Kind is what it would be added as: chat, claude or embedding for a
	// deployment, agent for an agent.
	Kind string `json:"kind"`
	// Model is the model behind it ("" when unknown).
	Model         string `json:"model"`
	ContextWindow int    `json:"contextWindow,omitempty"`
	// AlreadyAdded: the resource registers it already, as ModelID.
	AlreadyAdded bool   `json:"alreadyAdded"`
	ModelID      string `json:"modelId,omitempty"`
	// Deployment (agents) is the project deployment the agent runs on;
	// Workflow marks a workflow agent (a preview).
	Deployment string `json:"deployment,omitempty"`
	Workflow   bool   `json:"workflow,omitempty"`
}

// AzureDiscoveryView is POST /connections/{id}/discover. An Azure refusal
// (a missing role, a wrong project, a sign-in Entra ID refused) is a 200
// with Error and Code; a resource that cannot list its project is 400
// DISCOVERY_UNAVAILABLE.
type AzureDiscoveryView struct {
	Deployments []AzureDiscoveryItemView `json:"deployments"`
	Agents      []AzureDiscoveryItemView `json:"agents"`
	Warnings    []string                 `json:"warnings"`
	Error       string                   `json:"error,omitempty"`
	Code        string                   `json:"code,omitempty"`
}

// Discover lists what a saved resource's Foundry project offers — its
// deployments (each with the kind it would be added as) and its agents —
// marking what the resource already registers. It needs Entra ID (or the
// Azure CLI) and a project endpoint; nothing it learns reaches the caches
// the chat path reads.
func (s *AzureModelService) Discover(ctx context.Context, as AzureScope, connID string) (out AzureDiscoveryView, err error) {
	defer logger.Guard("AzureModels.Discover", &err)
	owner, err := s.ownerFor(as)
	if err != nil {
		return AzureDiscoveryView{}, err
	}
	s.prepareOwner(ctx, owner, as.Scope)
	if _, err := s.liveConnection(ctx, owner, connID); err != nil {
		return AzureDiscoveryView{}, connectionNotFound(err)
	}
	cred, err := s.credentialState(owner, as.Scope, connID)
	if err != nil {
		return AzureDiscoveryView{}, err
	}
	if !cred.ready() {
		return AzureDiscoveryView{}, azureRegErr(http.StatusBadRequest, AzureCodeDiscoveryUnavailable, cred.reason, ai.ErrAzureDiscoveryUnavailable, nil)
	}
	models, err := s.registry.ListModels(ctx, owner)
	if err != nil {
		return AzureDiscoveryView{}, err
	}
	d, derr := s.discover(ctx, cred.raw, registeredEntries(models, connID))
	out = AzureDiscoveryView{Deployments: []AzureDiscoveryItemView{}, Agents: []AzureDiscoveryItemView{}, Warnings: []string{}}
	var ce *ai.AzureConfigError
	switch {
	case derr == nil:
	case errors.Is(derr, ai.ErrAzureDiscoveryUnavailable):
		return AzureDiscoveryView{}, azureRegErr(http.StatusBadRequest, AzureCodeDiscoveryUnavailable, strings.TrimPrefix(derr.Error(), ai.ErrAzureDiscoveryUnavailable.Error()+": "), derr, nil)
	case errors.As(derr, &ce):
		return AzureDiscoveryView{}, derr
	default:
		out.Error = connectionTestError(ai.AzureFoundryProviderID, "the project", derr)
		out.Code = azureFailureCode(derr)
		return out, nil
	}
	for _, dep := range d.Deployments {
		out.Deployments = append(out.Deployments, AzureDiscoveryItemView{
			Name: dep.Name, Kind: dep.Kind, Model: dep.Model, ContextWindow: dep.ContextWindow,
			AlreadyAdded: dep.AlreadyAdded, ModelID: dep.ModelID,
		})
	}
	for _, a := range d.Agents {
		out.Agents = append(out.Agents, AzureDiscoveryItemView{
			Name: a.Name, Kind: a.Kind, Model: a.Model, ContextWindow: a.ContextWindow,
			AlreadyAdded: a.AlreadyAdded, ModelID: a.ModelID, Deployment: a.Deployment, Workflow: a.Workflow,
		})
	}
	out.Warnings = append(out.Warnings, d.Warnings...)
	return out, nil
}
