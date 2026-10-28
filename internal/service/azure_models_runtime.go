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

// The registry at run time: chat resolution, the picker, the KB embedder, tests and discovery — no read path waits on the network.

// azureDefaultWindow is the window baki assumes for a model nothing sizes (the Azure provider's ContextLimit).
const azureDefaultWindow = 128000

// Background refresh of project listings: at most azureRefreshParallel resources at once, each bounded by azureRefreshTimeout.
const (
	azureRefreshParallel = 4
	azureRefreshTimeout  = 45 * time.Second
)

// ---- Chat resolution ----

// AzureChatResolution is what a chat turn runs on for one model id; it travels with the turn — never through the by-value request.
type AzureChatResolution struct {
	// ResolvedModel is the provider-level id the turn sends and sizes by: "<deployment>", "claude:<deployment>" or "agent:<name>".
	ResolvedModel string
	// RequestedModel is the id the answer is persisted with: the request's, or for "" the default's "m/<entryId>".
	RequestedModel string
	// ModelLabel is the entry's display name at send time (a raw id: the id itself).
	ModelLabel string
	// Kind is the entry's kind: chat, claude or agent.
	Kind string
	// ConnectionRef names the credential the provider is built from: the caller's own resource or the organisation's.
	ConnectionRef ai.AzureConnectionRef
	// Entries is the resource's full entry set (the picker shares it, so both listing caches coincide).
	Entries []ai.AzureRegisteredModel
	// EntryID is the registry entry ("" for a raw id via the legacy document); Shared marks an org's entry.
	EntryID string
	Shared  bool
}

// ResolveChat resolves a chat turn's model id (see AzureChatResolution): personal entries first, then credOrg's — no other org is queried, so no existence oracle.
// Unusable ids are all ErrModelNotAvailable; registry/keystore failures and a pending legacy import are different errors, never a fallback.
// A resolved resource whose project listing is due gets a background refresh (refreshResolved).
func (s *AzureModelService) ResolveChat(ctx context.Context, scope, credOrg, modelID string) (res AzureChatResolution, err error) {
	defer logger.Guard("AzureModels.ResolveChat", &err)
	if res, err = s.resolveChat(ctx, scope, credOrg, modelID); err == nil {
		s.refreshResolved(ctx, scope, res)
	}
	return res, err
}

// resolveChat is ResolveChat's resolution.
func (s *AzureModelService) resolveChat(ctx context.Context, scope, credOrg, modelID string) (AzureChatResolution, error) {
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
			// No personal default, and the organisation's is its legacy document's chat deployment.
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

	// A raw (pre-registry) id: the caller's legacy document answers it first, so not while unimported.
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

// aliasMatch finds the entry of st's "default" resource whose pre-registry id is id: exactly, else case-insensitively.
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

// resolveEntry is the resolution of entry m of st (an embedding model is never a chat target).
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

// resolveLegacyID resolves a raw id no entry aliases on the caller's own "default" resource, as the legacy document did.
// A malformed one is not available.
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

// legacyDefaultTarget is the model the legacy document offered as its default.
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

// serverDefault is the entry a new chat starts on (member-first): the legacy-imported personal default,
// the first personal chat entry, the org's "Default for members", the first org chat entry.
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

// AzurePickerListing is the Azure part of the provider listing, built from the registry — network-free.
type AzurePickerListing struct {
	// Models are the caller's own and orgID's shared entries in display order, with their statuses.
	Models []models.ModelDetail
	// DefaultModel is the server default's "m/<id>" ("" when there is none).
	DefaultModel string
	// Configured: the caller has a resource of their own. Shared: they have none, and orgID has one.
	Configured bool
	Shared     bool
	// SuggestDiscovery offers the "your project has models you haven't added" prompt (project listable, prompt not dismissed, registry writable).
	SuggestDiscovery bool
}

// PickerListing builds the Azure part of the provider listing; role check is the handler's, stale listings refresh in the background.
// A pending legacy import never fails the listing, nor moves the caller onto the organisation.
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
			// Aliases (pre-registry ids) only on the "default" resource's entries; an org's only for a member without their own.
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

// windowsFor sizes st's chat/Claude/agent entries without the network, from the provider each ready resource runs on.
// reported leaves the users' overrides out (0 = nothing sizes it).
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
			if p, err := s.factory.ForAzureConnection(ctx, st.scope, ref, use); err == nil {
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

// azureCatalogueWindows is the Azure reference catalogue's windows by lower-cased model id.
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

// azureRefreshQueueMax bounds the refreshes waiting for a worker; past it a read's refresh is dropped.
const azureRefreshQueueMax = 64

// azureListingRefresher refreshes stale listings off the read paths, one job per resource (credential read at run time).
type azureListingRefresher struct {
	refresh  func(ctx context.Context, raw string, models []ai.AzureRegisteredModel) error
	mu       sync.Mutex
	running  int
	inflight map[string]bool
	queue    []azureRefreshJob
	wg       sync.WaitGroup
	// known is what the last credential read of each resource (by job key) showed, so the chat path decides
	// whether a refresh is due without reading the credential again (refreshResolved). Trusted for
	// azureRefreshKnownTTL; at azureRefreshKnownMax resources it starts over.
	known map[string]azureRefreshKnown
}

// azureRefreshKnown is what one credential read of a resource showed: whether it lists a project (ready, Entra ID
// or CLI + project endpoint) and its credential's fingerprint, at the service's clock.
type azureRefreshKnown struct {
	project     bool
	fingerprint string
	at          time.Time
}

// What the chat path trusts of an earlier credential read: for the listing TTL — a credential replaced on another
// replica is seen within it — and for at most this many resources.
const (
	azureRefreshKnownTTL = 5 * time.Minute
	azureRefreshKnownMax = 4096
)

// remember records what a credential read of key's resource showed.
func (r *azureListingRefresher) remember(key string, k azureRefreshKnown) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, had := r.known[key]; !had && len(r.known) >= azureRefreshKnownMax {
		r.known = map[string]azureRefreshKnown{}
	}
	r.known[key] = k
}

// recall is what the last credential read of key's resource showed, while it is trusted at now.
func (r *azureListingRefresher) recall(key string, now time.Time) (azureRefreshKnown, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	k, ok := r.known[key]
	if !ok || now.Sub(k.at) >= azureRefreshKnownTTL || now.Before(k.at) {
		return azureRefreshKnown{}, false
	}
	return k, true
}

// azureRefreshKey is the refresh job key of owner's resource connID: one job per resource, whoever triggers it.
func azureRefreshKey(owner storageif.AzureOwner, connID string) string {
	return owner.String() + "\x00" + connID
}

// azureRefreshJob is one resource's refresh: owner/scope/connID, the credential fingerprint the read saw, and the entries.
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
	return &azureListingRefresher{refresh: refresh, inflight: map[string]bool{}, known: map[string]azureRefreshKnown{}}
}

// trigger schedules job unless its resource's refresh is running or queued. Never blocks.
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
		// Clear the popped slot before reslicing: it would hold the job's entries and closure until reallocation.
		r.queue[0] = azureRefreshJob{}
		r.queue = r.queue[1:]
		r.mu.Unlock()
	}
}

// run refreshes one listing (bounded by azureRefreshTimeout) on the credential as stored NOW; a panic is logged.
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

// current reads the job's credential and reports whether it is still the one queued for (same fingerprint).
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

// refreshListings triggers a background refresh of st's resources whose listing is due (triggerRefresh).
func (s *AzureModelService) refreshListings(st *azureOwnerState) {
	if s.refresher == nil {
		return
	}
	for _, c := range st.conns {
		s.triggerRefresh(st.owner, st.scope, c.ID, st.creds[c.ID], st.entriesOf(c.ID))
	}
}

// refreshResolved triggers the background refresh of a chat turn's resource when its listing is due: registry
// providers only read the listing cache, and on a replica no picker or Settings read has warmed (or one idle past
// the stale grace) the turns would otherwise size from undecorated entries. What an earlier credential read showed
// (the refresher's memo) decides without the keystore — a live listing, a pending retry or a resource that lists
// no project costs no read; otherwise one read under the request's context. The refresh itself runs on the
// refresher, which reads the credential again when it runs, so the turn never waits on the project.
func (s *AzureModelService) refreshResolved(ctx context.Context, scope string, res AzureChatResolution) {
	if s.refresher == nil || !hasChatEntry(res.Entries) {
		return
	}
	owner := s.userOwner(scope)
	if res.ConnectionRef.OrgID != "" {
		owner = storageif.AzureOrgOwner(res.ConnectionRef.OrgID)
	}
	connID := res.ConnectionRef.ConnID
	if k, ok := s.refresher.recall(azureRefreshKey(owner, connID), s.now()); ok {
		if k.project && ai.AzureFoundryListingDue(k.fingerprint, res.Entries) {
			s.refresher.trigger(s.refreshJob(owner, scope, connID, k.fingerprint, res.Entries))
		}
		return
	}
	raw, err := s.readCredentialCtx(ctx, owner, scope, connID)
	cred, err := credentialFromRead(owner, connID, raw, err)
	if err != nil {
		logger.FromContext(ctx).Debug("azure registry: a chat turn's listing refresh could not read its credential; skipped", "owner", owner.String(), "resource", connID, "error", err)
		return
	}
	s.triggerRefresh(owner, scope, connID, cred, res.Entries)
}

// readCredentialCtx is readCredential under ctx, through the keystores' request-scoped reads where they are wired.
func (s *AzureModelService) readCredentialCtx(ctx context.Context, owner storageif.AzureOwner, scope, connID string) (string, error) {
	var (
		raw string
		err error
	)
	switch {
	case owner.IsOrg() && s.orgKeys != nil && s.orgKeyReads != nil:
		raw, err = s.orgKeyReads.GetOrgCtx(ctx, owner.ID, credentialKey(connID))
	case !owner.IsOrg() && s.keyReads != nil:
		raw, err = s.keyReads.GetCtx(ctx, scope, credentialKey(connID))
	default:
		return s.readCredential(owner, scope, connID)
	}
	if errors.Is(err, storage.ErrSecretNotFound) {
		return "", nil
	}
	return raw, err
}

// triggerRefresh records what this read of connID's credential showed (the chat path's memo) and schedules the
// resource's listing refresh when it lists a project — ready, Entra ID (or CLI) + project endpoint, at least one
// chat entry — and its listing is due (ai.AzureFoundryListingOf): a live listing, or a failed refresh's pending
// retry, costs nothing. The job key is the resource, so the picker's and the chat path's triggers share one job.
func (s *AzureModelService) triggerRefresh(owner storageif.AzureOwner, scope, connID string, cred azureCredential, entries []ai.AzureRegisteredModel) {
	project := cred.ready() && cred.cfg.ProjectAccess()
	known := azureRefreshKnown{project: project, at: s.now()}
	if project {
		known.fingerprint = cred.cfg.Fingerprint()
	}
	s.refresher.remember(azureRefreshKey(owner, connID), known)
	if !project || !hasChatEntry(entries) || !ai.AzureFoundryListingOf(cred.cfg, entries).Due {
		return
	}
	s.refresher.trigger(s.refreshJob(owner, scope, connID, known.fingerprint, entries))
}

// refreshJob is the refresh of owner's resource connID on the credential with fingerprint.
func (s *AzureModelService) refreshJob(owner storageif.AzureOwner, scope, connID, fingerprint string, entries []ai.AzureRegisteredModel) azureRefreshJob {
	return azureRefreshJob{
		key: azureRefreshKey(owner, connID), owner: owner, scope: scope, connID: connID,
		fingerprint: fingerprint, entries: entries, read: s.readCredential,
	}
}

// ---- The knowledge base's embedder ----

// azureEmbedderError is a KB embedder refusal with its own copy, sentinel and optional cause.
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

// KnowledgeEmbedder resolves the KB's embedder: an org corpus uses the org's designated model (none = refusal, never a personal fallback).
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
			p, err := s.factory.ForAzureConnectionEmbedding(ctx, scope, ai.AzureConnectionRef{OrgID: orgID, ConnID: m.ConnectionID}, registeredModel(m))
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
	p, err = s.factory.ForAzureConnectionEmbedding(ctx, scope, ai.AzureConnectionRef{ConnID: m.ConnectionID}, registeredModel(m))
	return knowledgeEmbedderResult(owner, m, p, err)
}

// knowledgeEmbedderResult classifies the factory's answer: config gaps keep their sentinels; the rest is unavailable.
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
		errors.Is(err, ai.ErrEmbeddingDeploymentMissing), errors.Is(err, ai.ErrOrgEmbeddingDeploymentMissing),
		errors.Is(err, ai.ErrProviderNotAllowed), errors.Is(err, ai.ErrProviderPolicyUnavailable):
		// The policy's answers are the admin's to act on (or its own fixed copy), not a transient setup failure.
		return nil, err
	}
	return nil, knowledgeEmbedderUnavailable(owner, err)
}

// knowledgeEmbedderUnavailable logs the cause and returns the transient refusal; the cause is never wrapped.
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

// AzureTestModelRef names the model of a test: a saved one by entry id, or a draft {kind, deployment}.
type AzureTestModelRef struct {
	ID         string `json:"id,omitempty"`
	Kind       string `json:"kind,omitempty"`
	Deployment string `json:"deployment,omitempty"`
}

// AzureTestInput is POST /test: {connectionId} tests a saved resource, {config} a draft one; model adds a model test.
type AzureTestInput struct {
	ConnectionID string                      `json:"connectionId,omitempty"`
	Config       *AzureConnectionConfigInput `json:"config,omitempty"`
	Model        *AzureTestModelRef          `json:"model,omitempty"`
}

// AzureTestResult is POST /test's answer for every test outcome; Code classifies a failure.
type AzureTestResult struct {
	OK        bool   `json:"ok"`
	LatencyMs int    `json:"latencyMs"`
	Error     string `json:"error,omitempty"`
	Code      string `json:"code,omitempty"`
}

// Test is the one test entry point (saved or draft resource/model); drafts validate before any network call.
func (s *AzureModelService) Test(ctx context.Context, as AzureScope, in AzureTestInput) (out AzureTestResult, err error) {
	defer logger.Guard("AzureModels.Test", &err)
	owner, err := s.ownerFor(as)
	if err != nil {
		return AzureTestResult{}, err
	}
	// The probes run around the factory: ask the policy here, as every builder does, before any credential is read.
	if err := s.admitProbe(ctx, as); err != nil {
		if code := connectionTestCode(err); code != "" {
			return AzureTestResult{OK: false, Error: err.Error(), Code: code}, nil
		}
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

// admitProbe asks the provider policy whether azure-foundry may be exercised for as (the deployment's disable,
// the organisation's list): the probes build a raw provider around the factory, so they ask themselves.
func (s *AzureModelService) admitProbe(ctx context.Context, as AzureScope) error {
	if s.factory == nil {
		return nil
	}
	return s.factory.Admit(ctx, as.Scope, as.OrgID, ai.AzureFoundryProviderID)
}

// savedModelID is the entry id a test result is recorded on ("" = the resource's).
func savedModelID(saved *storageif.AzureModelRecord, draft *ai.AzureRegisteredModel) string {
	if saved != nil && draft == nil {
		return saved.ID
	}
	return ""
}

// testModelRecord reads the saved model a test names (MODEL_NOT_FOUND when not the scope's).
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
	// Kind is what it would be added as: chat, claude or embedding for a deployment, agent for an agent.
	Kind string `json:"kind"`
	// Model is the model behind it ("" when unknown).
	Model         string `json:"model"`
	ContextWindow int    `json:"contextWindow,omitempty"`
	// AlreadyAdded: the resource registers it already, as ModelID.
	AlreadyAdded bool   `json:"alreadyAdded"`
	ModelID      string `json:"modelId,omitempty"`
	// Deployment (agents) is the project deployment the agent runs on; Workflow marks a workflow agent.
	Deployment string `json:"deployment,omitempty"`
	Workflow   bool   `json:"workflow,omitempty"`
}

// AzureDiscoveryView is POST /connections/{id}/discover: an Azure refusal is a 200 with Error and Code;
// a resource that cannot list its project is 400 DISCOVERY_UNAVAILABLE.
type AzureDiscoveryView struct {
	Deployments []AzureDiscoveryItemView `json:"deployments"`
	Agents      []AzureDiscoveryItemView `json:"agents"`
	Warnings    []string                 `json:"warnings"`
	Error       string                   `json:"error,omitempty"`
	Code        string                   `json:"code,omitempty"`
}

// Discover lists what a saved resource's Foundry project offers, marking what it already registers.
func (s *AzureModelService) Discover(ctx context.Context, as AzureScope, connID string) (out AzureDiscoveryView, err error) {
	defer logger.Guard("AzureModels.Discover", &err)
	owner, err := s.ownerFor(as)
	if err != nil {
		return AzureDiscoveryView{}, err
	}
	if err := s.admitProbe(ctx, as); err != nil {
		if code := connectionTestCode(err); code != "" {
			return AzureDiscoveryView{Deployments: []AzureDiscoveryItemView{}, Agents: []AzureDiscoveryItemView{}, Warnings: []string{}, Error: err.Error(), Code: code}, nil
		}
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
