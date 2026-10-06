package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"math/big"
	"net/http"
	"sort"
	"strings"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/platform/txhook"
	"pad-analyzer/internal/storage"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
	"pad-core/models"
)

// The legacy single document and the registry.
//
// Before Part 9 a user (and an organisation) kept ONE Azure AI Foundry document: the credential plus
// comma-separated deployment names. The registry imports it, lazily and once per owner: the document becomes
// resource "default" (its credential stays under the historical key) and each deployment it names an entry.
// For one release the single-document routes keep working on resource "default": they map the deployment fields
// onto registry entries and save only the credential part, carrying the old fields over unchanged — a rolled-back
// binary still finds them, and no field is ever added to a document (that binary's parser is strict).

// azureImportMemoTTL is how long an owner whose import marker was seen skips the marker read.
const azureImportMemoTTL = 60 * time.Second

// azureMemoPrune bounds the memo maps: past it, an insert prunes expired entries first.
const azureMemoPrune = 512

func (s *AzureModelService) importMemoFresh(owner storageif.AzureOwner) bool {
	s.memoMu.Lock()
	defer s.memoMu.Unlock()
	at, ok := s.importedMemo[owner]
	return ok && s.now().Sub(at) < azureImportMemoTTL
}

// recordImportMemo remembers owner's marker once its transaction committed — a rolled-back request must not hide the registry.
func (s *AzureModelService) recordImportMemo(ctx context.Context, owner storageif.AzureOwner) {
	txhook.Register(ctx, func(context.Context) {
		s.memoMu.Lock()
		defer s.memoMu.Unlock()
		now := s.now()
		if len(s.importedMemo) >= azureMemoPrune {
			for o, at := range s.importedMemo {
				if now.Sub(at) >= azureImportMemoTTL {
					delete(s.importedMemo, o)
				}
			}
		}
		s.importedMemo[owner] = now
	})
}

// forgetOwner drops what the service remembers of owner (its registry went with an account or organisation).
func (s *AzureModelService) forgetOwner(owner storageif.AzureOwner) {
	s.memoMu.Lock()
	defer s.memoMu.Unlock()
	delete(s.importedMemo, owner)
	delete(s.sweptMemo, owner)
	delete(s.deferredMemo, owner)
}

// importDeferredFresh reports whether owner's legacy import was deferred less than azureImportMemoTTL ago.
func (s *AzureModelService) importDeferredFresh(owner storageif.AzureOwner) bool {
	s.memoMu.Lock()
	defer s.memoMu.Unlock()
	at, ok := s.deferredMemo[owner]
	return ok && s.now().Sub(at) < azureImportMemoTTL
}

// setImportDeferred records (deferred) or clears owner's deferral.
func (s *AzureModelService) setImportDeferred(owner storageif.AzureOwner, deferred bool) {
	s.memoMu.Lock()
	defer s.memoMu.Unlock()
	if !deferred {
		delete(s.deferredMemo, owner)
		return
	}
	now := s.now()
	if len(s.deferredMemo) >= azureMemoPrune {
		for o, at := range s.deferredMemo {
			if now.Sub(at) >= azureImportMemoTTL {
				delete(s.deferredMemo, o)
			}
		}
	}
	s.deferredMemo[owner] = now
}

// azureLegacyState is what the legacy document of an owner is.
type azureLegacyState int

const (
	azureLegacyMissing azureLegacyState = iota
	azureLegacyReadable
	azureLegacyUnreadable
)

// readLegacyDocument reads owner's legacy document (the "azure-foundry" key): missing, readable, or unreadable —
// ONLY a decrypt or parse failure; any other keystore failure is returned and the import waits for the next read.
func (s *AzureModelService) readLegacyDocument(owner storageif.AzureOwner, scope string) (azureLegacyState, ai.AzureFoundryConfig, error) {
	raw, err := s.readCredential(owner, scope, ai.AzureDefaultConnectionID)
	switch {
	case errors.Is(err, storage.ErrSecretDecryptFailed):
		return azureLegacyUnreadable, ai.AzureFoundryConfig{}, nil
	case err != nil:
		return 0, ai.AzureFoundryConfig{}, err
	case strings.TrimSpace(raw) == "":
		return azureLegacyMissing, ai.AzureFoundryConfig{}, nil
	}
	cfg, perr := ai.ParseAzureFoundryConfig(raw)
	if perr != nil || !cfg.Configured() {
		return azureLegacyUnreadable, ai.AzureFoundryConfig{}, nil
	}
	return azureLegacyReadable, cfg, nil
}

// savedDefaultModel reads the caller's saved "Default for new chats": their own settings with per-user registries, the desktop's otherwise.
func (s *AzureModelService) savedDefaultModel(ctx context.Context, scope string) (string, error) {
	if s.perUserOwners {
		if s.userSettings == nil || scope == "" {
			return "", nil
		}
		stored, err := s.userSettings.LoadUserSettings(ctx, scope)
		if errors.Is(err, storageif.ErrNotFound) || (err == nil && stored == nil) {
			return "", nil
		}
		if err != nil {
			return "", err
		}
		return stored.AI.Providers[ai.AzureFoundryProviderID].DefaultModel, nil
	}
	if s.settings == nil {
		return "", nil
	}
	if st := s.settings.Get(); st != nil {
		return st.AI.Providers[ai.AzureFoundryProviderID].DefaultModel, nil
	}
	return "", nil
}

// ensureLegacyImported imports owner's legacy document into the registry once per owner (Part 9 "Legacy import"):
// resource "default" (created even for an unreadable document, whose status then says so) and one entry per
// deployment it names — chat, additional, Claude, embedding, and for a user the saved default model when it
// names an unlisted deployment. Entry ids are deterministic. It runs as the system (no audit row) under the
// owner lock (marker re-checked), stops at the model limit (logged) and never fails the read: any keystore or
// database failure skips it without marker or memo and the next read retries — as does a document whose
// "default" cannot be created yet (limit reached, or no name free).
//
// It reports whether owner is IMPORTED (marker present, or no legacy document); false means PENDING — the read
// paths must not answer as if the document did not exist (see errAzureImportPending).
func (s *AzureModelService) ensureLegacyImported(ctx context.Context, owner storageif.AzureOwner, scope string) (imported bool) {
	if s.importMemoFresh(owner) {
		return true
	}
	_, done, err := s.registry.ImportedAt(ctx, owner)
	if err != nil {
		logger.Warn("azure registry: reading the import marker failed; the import waits for the next read", "owner", owner.String(), "error", err)
		return !s.legacyDocumentMayExist(owner, scope)
	}
	if done {
		s.recordImportMemo(ctx, owner)
		return true
	}
	if s.importDeferredFresh(owner) {
		// Waiting for a free slot (or name): retried after the backoff, or at once when a resource is removed.
		return false
	}
	// Read before the owner lock: a keystore read can take its timeout, and nothing may wait on the lock for it.
	state, cfg, err := s.readLegacyDocument(owner, scope)
	if err != nil {
		logger.Warn("azure registry: reading the legacy configuration failed; the import waits for the next read", "owner", owner.String(), "error", err)
		return !s.legacyDocumentMayExist(owner, scope)
	}
	savedDefault := ""
	if !owner.IsOrg() && state == azureLegacyReadable {
		if savedDefault, err = s.savedDefaultModel(ctx, scope); err != nil {
			logger.Warn("azure registry: reading the saved default model failed; the import waits for the next read", "owner", owner.String(), "error", err)
			return false
		}
	}
	err = s.registry.WithOwnerLock(ctx, owner, func(lctx context.Context) error {
		if _, done, err := s.registry.ImportedAt(lctx, owner); err != nil || done {
			return err
		}
		if err := s.importLegacyLocked(lctx, owner, state, cfg, savedDefault); err != nil {
			return err
		}
		_, err := s.registry.MarkImported(lctx, owner)
		return err
	})
	switch {
	case errors.Is(err, errLegacyImportDeferred):
		logger.Warn("azure registry: the legacy configuration waits for a free resource slot (or a free name) for its resource \"default\"; retried once a resource is removed", "owner", owner.String())
		s.setImportDeferred(owner, true)
		return false
	case err != nil:
		logger.Warn("azure registry: importing the legacy configuration failed; retried on the next read", "owner", owner.String(), "error", err)
		// Nothing to import is nothing pending, marker or not.
		return state == azureLegacyMissing
	}
	s.setImportDeferred(owner, false)
	s.recordImportMemo(ctx, owner)
	return true
}

// errLegacyImportDeferred: the legacy document's resource "default" cannot be created yet (limit reached, or
// every candidate name taken). No marker is written; retried after a minute's backoff, or once a resource is removed.
var errLegacyImportDeferred = errors.New("azure registry: the legacy import's resource \"default\" cannot be created yet")

// errAzureImportPending is the answer of a read path that cannot answer while an owner's legacy import is pending:
// the server default and a raw pre-registry id resolve through the legacy document, and the KB's embedder may be
// its embedding deployment. Never ErrModelNotAvailable (the client would re-pin the thread) and never another
// owner's model or credential; the chat maps it to its generic setup error, the KB to a 503 — both retried.
var errAzureImportPending = errors.New("the Azure AI Foundry configuration is still being moved to the model registry — try again in a moment")

// legacyDocumentMayExist reports whether owner may have a legacy document; a keystore failure counts as yes (the import stays pending).
func (s *AzureModelService) legacyDocumentMayExist(owner storageif.AzureOwner, scope string) bool {
	var (
		has bool
		err error
	)
	if owner.IsOrg() {
		if s.orgKeys == nil {
			return false
		}
		has, err = s.orgKeys.HasOrg(owner.ID, ai.AzureFoundryProviderID)
	} else {
		has, err = s.secrets.Has(scope, ai.AzureFoundryProviderID)
	}
	if err != nil {
		logger.Warn("azure registry: checking for a legacy configuration failed; its import counts as pending", "owner", owner.String(), "error", err)
		return true
	}
	return has
}

// importLegacyLocked writes the import (owner lock held); idempotent — a retry finds the rows it already wrote.
// A "default" that cannot be created yet is errLegacyImportDeferred (no marker).
func (s *AzureModelService) importLegacyLocked(ctx context.Context, owner storageif.AzureOwner, state azureLegacyState, cfg ai.AzureFoundryConfig, savedDefault string) error {
	if state == azureLegacyMissing {
		return nil
	}
	name := "Azure AI Foundry"
	if state == azureLegacyReadable {
		if n := connectionNameFromHost(cfg.Endpoint); validAzureText(n, n, 1, storageif.AzureNameMaxLen) {
			name = n
		}
	}
	ok, err := s.importDefaultConnection(ctx, owner, name)
	if err != nil {
		return err
	}
	if !ok {
		return errLegacyImportDeferred
	}
	if state == azureLegacyUnreadable {
		return nil
	}
	for _, rec := range legacyEntryRecords(owner, cfg, savedDefault) {
		_, err := s.registry.ImportModel(ctx, owner, rec)
		switch {
		case errors.Is(err, storageif.ErrAzureTooManyModels):
			logger.Warn("azure registry: the legacy import stopped at the model limit", "owner", owner.String(), "limit", storageif.AzureMaxModels)
			return nil
		case errors.Is(err, storageif.ErrAzureRegistryInvalid):
			logger.Warn("azure registry: skipping a legacy deployment the registry refuses", "owner", owner.String(), "deployment", rec.Deployment, "error", err)
		case err != nil:
			return err
		}
	}
	return nil
}

// defaultConnectionNames are the names resource "default" is created under, in order: name, then name-2 … name-9.
func defaultConnectionNames(name string) []string {
	out := []string{name}
	for i := 2; i <= 9; i++ {
		out = append(out, truncateRunes(name, storageif.AzureNameMaxLen-2)+fmt.Sprintf("-%d", i))
	}
	return out
}

// importDefaultConnection creates resource "default" (idempotently): false, with no error, when it cannot exist
// yet — the limit is reached, or every candidate name is taken (the callers decide what that means).
func (s *AzureModelService) importDefaultConnection(ctx context.Context, owner storageif.AzureOwner, name string) (bool, error) {
	// A retry after a partial import finds it — even at the limit, which the stores check before any uniqueness rule.
	if _, err := s.registry.GetConnection(ctx, owner, ai.AzureDefaultConnectionID); err == nil {
		return true, nil
	} else if !errors.Is(err, storageif.ErrAzureConnectionNotFound) {
		return false, err
	}
	for _, candidate := range defaultConnectionNames(name) {
		created, err := s.registry.ImportConnection(ctx, owner, storageif.AzureConnectionRecord{ID: ai.AzureDefaultConnectionID, Name: candidate})
		if errors.Is(err, storageif.ErrAzureTooManyConnections) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		if created {
			return true, nil
		}
		// Skipped: resource "default" exists (a retry), or another resource has the name.
		if _, gerr := s.registry.GetConnection(ctx, owner, ai.AzureDefaultConnectionID); gerr == nil {
			return true, nil
		} else if !errors.Is(gerr, storageif.ErrAzureConnectionNotFound) {
			return false, gerr
		}
	}
	return false, nil
}

// legacyAzureEntryID is an imported entry's deterministic id: the first ten base-36 characters of
// sha256("azure-legacy\0"+owner+"\0"+ns+"\0"+lower(name)) — the same on every retry and replica, so an
// import that ran twice meets itself at the primary key.
func legacyAzureEntryID(owner storageif.AzureOwner, ns, name string) string {
	sum := sha256.Sum256([]byte("azure-legacy\x00" + owner.String() + "\x00" + ns + "\x00" + strings.ToLower(name)))
	s := new(big.Int).SetBytes(sum[:]).Text(36)
	for len(s) < 10 {
		s = "0" + s
	}
	return s[:10]
}

// azureLegacyEntry is one entry an import (or a single-document save) makes.
type azureLegacyEntry struct {
	kind string
	name string
	kb   bool // the knowledge-base model
	def  bool // an organisation's default for members
}

// legacyEntrySpecs lists the entries cfg's deployment fields name, savedDefault included for a user.
func legacyEntrySpecs(org bool, cfg ai.AzureFoundryConfig, savedDefault string) []azureLegacyEntry {
	defaultTarget := legacyDefaultTarget(cfg)
	var out []azureLegacyEntry
	add := func(kind, name string, kb bool) {
		if name == "" {
			return
		}
		target, _ := ai.AzureModelTarget(kind, name)
		out = append(out, azureLegacyEntry{kind: kind, name: name, kb: kb, def: org && target != "" && target == defaultTarget})
	}
	add(storageif.AzureKindChat, cfg.ChatDeployment, false)
	for _, d := range cfg.Deployments {
		add(storageif.AzureKindChat, d, false)
	}
	for _, d := range cfg.ClaudeDeployments {
		add(storageif.AzureKindClaude, d, false)
	}
	if cfg.EmbeddingDeployment != "" {
		add(storageif.AzureKindEmbedding, cfg.EmbeddingDeployment, true)
	}
	if org {
		return out
	}
	listed := func(kinds []string, name string) bool {
		for _, e := range out {
			for _, k := range kinds {
				if e.kind == k && strings.EqualFold(e.name, name) {
					return true
				}
			}
		}
		return false
	}
	switch saved := strings.TrimSpace(savedDefault); {
	case saved == "" || strings.HasPrefix(saved, "m/"):
	case strings.HasPrefix(saved, "agent:"):
		if name := strings.TrimPrefix(saved, "agent:"); ai.ValidAzureAgentName(name) {
			add(storageif.AzureKindAgent, name, false)
		}
	case strings.HasPrefix(saved, "claude:"):
		if name := strings.TrimPrefix(saved, "claude:"); ai.ValidAzureDeploymentName(name) && !listed([]string{storageif.AzureKindChat, storageif.AzureKindClaude}, name) {
			add(storageif.AzureKindClaude, name, false)
		}
	default:
		if ai.ValidAzureDeploymentName(saved) && !listed([]string{storageif.AzureKindChat, storageif.AzureKindClaude}, saved) {
			add(storageif.AzureKindChat, saved, false)
		}
	}
	return out
}

// legacyEntryRecords are legacyEntrySpecs as registry records of resource "default".
func legacyEntryRecords(owner storageif.AzureOwner, cfg ai.AzureFoundryConfig, savedDefault string) []storageif.AzureModelRecord {
	specs := legacyEntrySpecs(owner.IsOrg(), cfg, savedDefault)
	out := make([]storageif.AzureModelRecord, 0, len(specs))
	for _, e := range specs {
		out = append(out, legacyEntryRecord(owner, e))
	}
	return out
}

func legacyEntryRecord(owner storageif.AzureOwner, e azureLegacyEntry) storageif.AzureModelRecord {
	display := truncateRunes(cleanAzureText(e.name), storageif.AzureNameMaxLen)
	if !validAzureText(display, display, 1, storageif.AzureNameMaxLen) {
		display = truncateRunes(e.name, storageif.AzureNameMaxLen)
	}
	return storageif.AzureModelRecord{
		ID:                   legacyAzureEntryID(owner, storageif.AzureModelNamespace(e.kind), e.name),
		ConnectionID:         ai.AzureDefaultConnectionID,
		Kind:                 e.kind,
		Deployment:           e.name,
		DisplayName:          display,
		Purpose:              storageif.AzurePurposeGeneral,
		IsKnowledgeEmbedding: e.kb,
		IsDefault:            e.def,
	}
}

// ---- The single-document routes on resource "default" (one release) ----

// legacyPublic is doc's public view with the deployment fields built from the registry's entries of resource
// "default" — never from the document's stale fields: the chat deployment is the entry of the document's own
// chat deployment when still registered (else the first chat entry in display order), the additional deployments
// the other chat entries, the Claude deployments the Claude entries, the embedding deployment the KB model.
func (s *AzureModelService) legacyPublic(ctx context.Context, owner storageif.AzureOwner, doc ai.AzureFoundryConfig) (ai.AzureFoundryPublicConfig, error) {
	pub := doc.Public()
	pub.ChatDeployment, pub.EmbeddingDeployment = "", ""
	pub.Deployments, pub.ClaudeDeployments = nil, nil
	all, err := s.registry.ListModels(ctx, owner)
	if err != nil {
		return ai.AzureFoundryPublicConfig{}, err
	}
	var chatRecs []storageif.AzureModelRecord
	embedding := ""
	for _, m := range all {
		if m.ConnectionID != ai.AzureDefaultConnectionID {
			continue
		}
		switch m.Kind {
		case storageif.AzureKindChat:
			chatRecs = append(chatRecs, m)
		case storageif.AzureKindClaude:
			pub.ClaudeDeployments = append(pub.ClaudeDeployments, m.Deployment)
		case storageif.AzureKindEmbedding:
			if m.IsKnowledgeEmbedding || embedding == "" {
				embedding = m.Deployment
			}
		}
	}
	pub.EmbeddingDeployment = embedding
	// Display order, the order the server default picks in when the saved chat deployment is gone.
	sort.SliceStable(chatRecs, func(i, j int) bool {
		a, b := chatRecs[i], chatRecs[j]
		return azureEntryLess(a.Purpose, a.PurposeCustom, false, a.DisplayName, a.ID, b.Purpose, b.PurposeCustom, false, b.DisplayName, b.ID)
	})
	chats := make([]string, 0, len(chatRecs))
	for _, m := range chatRecs {
		chats = append(chats, m.Deployment)
	}
	chatIdx := 0
	for i, c := range chats {
		if doc.ChatDeployment != "" && strings.EqualFold(c, doc.ChatDeployment) {
			chatIdx = i
			break
		}
	}
	for i, c := range chats {
		if i == chatIdx {
			pub.ChatDeployment = c
			continue
		}
		pub.Deployments = append(pub.Deployments, c)
	}
	return pub, nil
}

// legacyGetConfig is GetAzureFoundryConfig on the registry: the default resource's document, deployment fields
// from the registry; a missing or unreadable document reads as unconfigured.
func (s *AzureModelService) legacyGetConfig(ctx context.Context, scope string) (ai.AzureFoundryPublicConfig, error) {
	owner := s.userOwner(scope)
	s.prepareOwner(ctx, owner, scope)
	state, doc, err := s.readLegacyDocument(owner, scope)
	if err != nil || state != azureLegacyReadable {
		if err != nil {
			logger.Warn("azure ai foundry: stored configuration unreadable", "error", err)
		}
		return ai.AzureFoundryPublicConfig{}, nil
	}
	return s.legacyPublic(ctx, owner, doc)
}

// legacyOrgGetConfig is GetOrgAzureFoundryConfig on the registry; an unreadable document is ErrOrgCredentialUnreadable.
func (s *AzureModelService) legacyOrgGetConfig(ctx context.Context, orgID string) (OrgAzureFoundryConfig, error) {
	owner := storageif.AzureOrgOwner(orgID)
	s.ensureLegacyImported(ctx, owner, "")
	raw, getErr := s.orgKeys.GetOrg(orgID, ai.AzureFoundryProviderID)
	if errors.Is(getErr, storage.ErrSecretNotFound) || (getErr == nil && raw == "") {
		return OrgAzureFoundryConfig{}, nil
	}
	if getErr != nil {
		return OrgAzureFoundryConfig{}, fmt.Errorf("%w: %w", ErrOrgCredentialUnreadable, getErr)
	}
	doc, parseErr := ai.ParseAzureFoundryConfig(raw)
	if parseErr != nil {
		return OrgAzureFoundryConfig{}, fmt.Errorf("%w: %w", ErrOrgCredentialUnreadable, parseErr)
	}
	pub, err := s.legacyPublic(ctx, owner, doc)
	if err != nil {
		return OrgAzureFoundryConfig{}, err
	}
	meta, hasMeta, metaErr := s.orgKeys.OrgKeyMeta(orgID, ai.AzureFoundryProviderID)
	if metaErr != nil {
		return OrgAzureFoundryConfig{}, fmt.Errorf("%w: %w", ErrOrgCredentialUnreadable, metaErr)
	}
	out := OrgAzureFoundryConfig{AzureFoundryPublicConfig: pub, Shared: true}
	if hasMeta {
		out.UpdatedByID = meta.UpdatedBy
		out.SecretSetByID = meta.SecretSetBy
		if out.SecretSetByID == "" {
			out.SecretSetByID = meta.UpdatedBy // a row no save has stamped since v24
		}
		if !meta.UpdatedAt.IsZero() {
			out.UpdatedAt = formatOrgKeyStamp(meta.UpdatedAt)
		}
	}
	return out, nil
}

// The single-document routes store the document exactly as saved, deployment fields included: they are that
// route's own fields, a binary rolled back past the registry finds what the route last saved, and the server
// default keeps following the saved chat deployment. Only these routes write the fields; a credential saved
// through the registry carries them over unchanged, and no other document ever has them.

// The single-document saves span the keystore and the registry with no transaction across the two — the same
// order as a registry credential change: everything the entry sync can be refused for is checked first
// (checkLegacySync), the previous document snapshotted, the document saved, the entries synced; a sync that
// still fails puts the previous document back (an organisation's with its writer, lender and stamp) — or, when
// it cannot, returns an AzureCredentialChangedError the handler audits.

// legacySaveConfig is SaveAzureFoundryConfig on the registry: the route's own validation, the document saved to
// the default resource as is, then its deployment fields mapped onto that resource's entries.
func (s *AzureModelService) legacySaveConfig(ctx context.Context, scope string, cfg ai.AzureFoundryConfig) error {
	owner := s.userOwner(scope)
	s.prepareOwner(ctx, owner, scope)
	cfg.Normalize()
	prev, hadPrev := storedAzureConfig(func() (string, error) { return s.readCredential(owner, scope, ai.AzureDefaultConnectionID) })
	if refusal := carryAzureSecret(&cfg, prev, hadPrev); refusal != nil {
		return saveCarryError(cfg, refusal)
	}
	if err := ai.ValidateAzureFoundryConfig(cfg); err != nil {
		return err
	}
	raw, err := cfg.Encode()
	if err != nil {
		return err
	}
	if err := s.checkLegacySync(ctx, owner, cfg); err != nil {
		return err
	}
	snap, err := s.snapshotCredential(owner, scope, ai.AzureDefaultConnectionID)
	if err != nil {
		return err
	}
	if err := s.secrets.Save(scope, ai.AzureFoundryProviderID, raw); err != nil {
		return err
	}
	if err := s.syncLegacyEntries(ctx, owner, cfg); err != nil {
		return s.undoCredentialSave(owner, scope, ai.AzureDefaultConnectionID, snap, time.Time{}, forgetReplacement(prev, hadPrev, cfg), err)
	}
	ai.InvalidateAzureFoundryModels(cfg)
	if hadPrev && prev.Fingerprint() != cfg.Fingerprint() {
		ai.ForgetAzureFoundryCredential(prev)
	}
	return nil
}

// legacyOrgSaveConfig is SaveOrgAzureFoundryConfig on the registry: the same checks (CLI refused, CAS on
// updatedAt, unreadable-row repair), the credential saved, fields mapped onto the org's default resource.
func (s *AzureModelService) legacyOrgSaveConfig(ctx context.Context, orgID, updatedBy string, expected time.Time, cfg ai.AzureFoundryConfig) (string, error) {
	owner := storageif.AzureOrgOwner(orgID)
	s.ensureLegacyImported(ctx, owner, "")
	if expected.IsZero() && cfg.HasSecret() {
		stamp, repair, rerr := unreadableOrgStamp(s.orgKeys, orgID, ai.AzureFoundryProviderID)
		if rerr != nil {
			return "", rerr
		}
		if repair {
			expected = stamp
		}
	}
	prev, hadPrev := storedAzureConfig(func() (string, error) { return s.orgKeys.GetOrg(orgID, ai.AzureFoundryProviderID) })
	supplied := cfg.HasSecret()
	if refusal := carryAzureSecret(&cfg, prev, hadPrev); refusal != nil {
		return "", saveCarryError(cfg, refusal)
	}
	if err := ai.ValidateAzureFoundryConfig(cfg); err != nil {
		return "", err
	}
	raw, err := cfg.Encode()
	if err != nil {
		return "", err
	}
	if err := s.checkLegacySync(ctx, owner, cfg); err != nil {
		return "", err
	}
	snap, err := s.snapshotCredential(owner, "", ai.AzureDefaultConnectionID)
	if err != nil {
		return "", err
	}
	if !snap.stamp().Equal(expected) {
		// The compare-and-swap below would refuse it: refuse before it.
		return "", ErrOrgCredentialConflict
	}
	at, ok, err := s.orgKeys.SaveOrgIfUnchanged(orgID, ai.AzureFoundryProviderID, raw, updatedBy, !supplied, expected)
	if err != nil {
		return "", err
	}
	if !ok {
		return "", ErrOrgCredentialConflict
	}
	if err := s.syncLegacyEntries(ctx, owner, cfg); err != nil {
		return "", s.undoCredentialSave(owner, "", ai.AzureDefaultConnectionID, snap, at, forgetReplacement(prev, hadPrev, cfg), err)
	}
	ai.InvalidateAzureFoundryModels(cfg)
	if hadPrev && prev.Fingerprint() != cfg.Fingerprint() {
		ai.ForgetAzureFoundryCredential(prev)
	}
	return formatOrgKeyStamp(at), nil
}

// forgetReplacement releases what the process may have kept for saved, a document stored for a moment and now replaced.
func forgetReplacement(prev ai.AzureFoundryConfig, hadPrev bool, saved ai.AzureFoundryConfig) func() {
	return func() {
		if !hadPrev || prev.Fingerprint() != saved.Fingerprint() {
			ai.ForgetAzureFoundryCredential(saved)
		}
	}
}

// legacyDefaultName is the name resource "default" gets for cfg: its endpoint host's first label, else a fixed name.
func legacyDefaultName(cfg ai.AzureFoundryConfig) string {
	if name := connectionNameFromHost(cfg.Endpoint); validAzureText(name, name, 1, storageif.AzureNameMaxLen) {
		return name
	}
	return "Azure AI Foundry"
}

// checkLegacySync refuses, before anything is written, what syncLegacyEntries would be refused for: a read-only
// registry, a missing resource "default" with no free slot or name, or more models than the limit once the
// fields' drops and adds are settled.
func (s *AzureModelService) checkLegacySync(ctx context.Context, owner storageif.AzureOwner, cfg ai.AzureFoundryConfig) error {
	if s.registry.State(ctx).State == storageif.AzureRegistryReadOnly {
		return storageif.ErrAzureRegistryReadOnly
	}
	conns, err := s.registry.ListConnections(ctx, owner)
	if err != nil {
		return err
	}
	hasDefault := false
	for _, c := range conns {
		hasDefault = hasDefault || c.ID == ai.AzureDefaultConnectionID
	}
	if !hasDefault {
		if len(conns) >= storageif.AzureMaxConnections {
			return tooManyConnections()
		}
		free := false
		for _, candidate := range defaultConnectionNames(legacyDefaultName(cfg)) {
			taken, err := s.registry.ConnectionNameTaken(ctx, owner, candidate, ai.AzureDefaultConnectionID)
			if err != nil {
				return err
			}
			if !taken {
				free = true
				break
			}
		}
		if !free {
			return azureRegErr(http.StatusConflict, AzureCodeDuplicateConnectionName,
				fmt.Sprintf("the configuration's resource cannot be named: %q and its numbered variants are all taken — rename one of your resources", legacyDefaultName(cfg)),
				storageif.ErrAzureDuplicateConnectionName, nil)
		}
	}
	models, err := s.registry.ListModels(ctx, owner)
	if err != nil {
		return err
	}
	key := func(kind, name string) string {
		return storageif.AzureModelNamespace(kind) + "\x00" + strings.ToLower(name)
	}
	want := map[string]bool{}
	for _, e := range legacyEntrySpecs(owner.IsOrg(), cfg, "") {
		want[key(e.kind, e.name)] = true
	}
	count := len(models)
	for _, m := range models {
		if m.ConnectionID != ai.AzureDefaultConnectionID || m.Kind == storageif.AzureKindAgent {
			continue
		}
		k := key(m.Kind, m.Deployment)
		if want[k] {
			delete(want, k) // kept (a kind change in place at most)
		} else {
			count-- // dropped
		}
	}
	if count+len(want) > storageif.AzureMaxModels {
		return tooManyModels()
	}
	return nil
}

// syncLegacyEntries maps a single-document save's deployment fields straight onto resource "default" (created
// when missing): missing entries are created (deterministic ids), entries no longer named are deleted, a chat
// entry named as a Claude deployment (or the reverse) changes kind in place; agents stay. The embedding
// deployment becomes the knowledge-base model, and for an organisation the document's default the default for
// members, when nothing else holds them.
func (s *AzureModelService) syncLegacyEntries(ctx context.Context, owner storageif.AzureOwner, cfg ai.AzureFoundryConfig) error {
	specs := legacyEntrySpecs(owner.IsOrg(), cfg, "")
	return s.registry.WithOwnerLock(ctx, owner, func(lctx context.Context) error {
		if _, err := s.registry.GetConnection(lctx, owner, ai.AzureDefaultConnectionID); errors.Is(err, storageif.ErrAzureConnectionNotFound) {
			ok, err := s.importDefaultConnection(lctx, owner, legacyDefaultName(cfg))
			if err != nil {
				return err
			}
			if !ok {
				return storageif.ErrAzureTooManyConnections
			}
		} else if err != nil {
			return err
		}
		models, err := s.registry.ListModels(lctx, owner)
		if err != nil {
			return err
		}
		want := map[string]azureLegacyEntry{}
		for _, e := range specs {
			want[storageif.AzureModelNamespace(e.kind)+"\x00"+strings.ToLower(e.name)] = e
		}
		have := map[string]bool{}
		for _, m := range models {
			if m.ConnectionID != ai.AzureDefaultConnectionID || m.Kind == storageif.AzureKindAgent {
				continue
			}
			key := storageif.AzureModelNamespace(m.Kind) + "\x00" + strings.ToLower(m.Deployment)
			e, keep := want[key]
			if !keep {
				if _, err := s.registry.DeleteModel(lctx, owner, m.ID); err != nil {
					return err
				}
				continue
			}
			have[key] = true
			if e.kind != m.Kind {
				_, err := s.registry.UpdateModel(lctx, owner, m.ID, storageif.AzureModelUpdate{
					ConnectionID: m.ConnectionID, Kind: e.kind, Deployment: m.Deployment, DisplayName: m.DisplayName,
					Purpose: m.Purpose, PurposeCustom: m.PurposeCustom, Description: m.Description,
					ContextWindow: m.ContextWindow, MaxOutputTokens: m.MaxOutputTokens,
				})
				if err != nil {
					return err
				}
			}
		}
		for _, e := range specs {
			key := storageif.AzureModelNamespace(e.kind) + "\x00" + strings.ToLower(e.name)
			if have[key] {
				continue
			}
			rec := legacyEntryRecord(owner, e)
			rec.IsKnowledgeEmbedding, rec.IsDefault = false, false // moved below, when free
			if _, err := s.registry.CreateModel(lctx, owner, rec); errors.Is(err, storageif.ErrAzureDuplicateModelID) {
				if rec.ID, err = newAzureRegistryID(); err != nil {
					return err
				}
				if _, err := s.registry.CreateModel(lctx, owner, rec); err != nil {
					return err
				}
			} else if err != nil {
				return err
			}
			have[key] = true
		}
		return s.settleLegacyDesignations(lctx, owner, specs)
	})
}

// settleLegacyDesignations gives the KB and default-for-members designations to the entries a save names for them.
func (s *AzureModelService) settleLegacyDesignations(ctx context.Context, owner storageif.AzureOwner, specs []azureLegacyEntry) error {
	models, err := s.registry.ListModels(ctx, owner)
	if err != nil {
		return err
	}
	find := func(e azureLegacyEntry) string {
		for _, m := range models {
			if m.ConnectionID == ai.AzureDefaultConnectionID && storageif.AzureModelNamespace(m.Kind) == storageif.AzureModelNamespace(e.kind) && strings.EqualFold(m.Deployment, e.name) {
				return m.ID
			}
		}
		return ""
	}
	for _, e := range specs {
		switch {
		case e.kb && designationHolder(models, true) == "":
			if id := find(e); id != "" {
				if err := s.registry.SetDesignation(ctx, owner, storageif.AzureDesignationKnowledgeEmbedding, id); err != nil {
					return err
				}
			}
		case e.def && owner.IsOrg() && designationHolder(models, false) == "":
			if id := find(e); id != "" {
				if err := s.registry.SetDesignation(ctx, owner, storageif.AzureDesignationOrgDefault, id); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

// legacyDeleteDefault is the single-document delete: resource "default" with its models, credential after the
// commit — or, with no such resource, the document alone.
func (s *AzureModelService) legacyDeleteDefault(ctx context.Context, owner storageif.AzureOwner, scope string) error {
	s.prepareOwner(ctx, owner, scope)
	_, err := s.deleteConnection(ctx, owner, scope, ai.AzureDefaultConnectionID, AzureDeleteOptions{Cascade: true, Force: true})
	if !errors.Is(err, storageif.ErrAzureConnectionNotFound) {
		return err
	}
	prev, hadPrev := storedAzureConfig(func() (string, error) { return s.readCredential(owner, scope, ai.AzureDefaultConnectionID) })
	if err := s.deleteCredential(owner, scope, ai.AzureDefaultConnectionID); err != nil {
		return err
	}
	if hadPrev {
		ai.ForgetAzureFoundryCredential(prev)
	}
	// Nothing left to import: an import waiting for a slot is over.
	s.setImportDeferred(owner, false)
	return nil
}

// legacyTest is the single-document connection test: resource "default", else the first resource, tested as a
// resource — an authenticated probe, no model call.
func (s *AzureModelService) legacyTest(ctx context.Context, as AzureScope) (*models.ProviderTestResult, error) {
	owner, err := s.ownerFor(as)
	if err != nil {
		return nil, err
	}
	s.prepareOwner(ctx, owner, as.Scope)
	st, err := s.loadOwner(ctx, owner, as.Scope, nil, false)
	if err != nil {
		return nil, err
	}
	if len(st.conns) == 0 {
		return &models.ProviderTestResult{Ok: false, Error: "no Azure AI Foundry resource is configured — add one under Settings → AI Accounts"}, nil
	}
	id := st.conns[0].ID
	if _, ok := st.connection(ai.AzureDefaultConnectionID); ok {
		id = ai.AzureDefaultConnectionID
	}
	res, err := s.Test(ctx, as, AzureTestInput{ConnectionID: id})
	if err != nil {
		return &models.ProviderTestResult{Ok: false, Error: err.Error()}, nil
	}
	return &models.ProviderTestResult{Ok: res.OK, Latency: res.LatencyMs, Error: res.Error}, nil
}
