package filesystem

import (
	"cmp"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// LocalStorageBackend implements StorageBackend for local file system storage
type LocalStorageBackend struct {
	dataDir      string
	mu           sync.RWMutex // guards users, orgs, and sharing maps
	flowMu       sync.Mutex   // guards SaveFlow version-bump + write (OCC)
	triageMu     sync.Mutex   // guards finding-status read-modify-write
	apiTokenMu   sync.Mutex   // guards api-token directory read-modify-write
	userTokenMu  sync.Mutex   // guards one-shot user tokens
	commentsMu   sync.Mutex   // guards finding-comment read-modify-write
	shareTokenMu sync.Mutex   // guards share-token read-modify-write
	govAlertMu   sync.Mutex   // guards governance-alerts read-modify-write
	users        map[string]*interfaces.User
	usersByEmail map[string]string // lowercased email → user ID; guarded by mu
	orgs         map[string]*interfaces.Organisation
	sharing      map[string][]*interfaces.Collaborator
	apiTokens    map[string]*interfaces.APIToken  // guarded by apiTokenMu
	userTokens   map[string]*interfaces.UserToken // keyed by token hash; guarded by userTokenMu
	// flowMeta caches listing metadata per flow ID (see flowMetaEntry);
	// guarded by metaMu. Invalidated by Stat (mtime+size) on refresh and kept
	// current by SaveFlow/DeleteFlow.
	metaMu   sync.RWMutex
	flowMeta map[string]*flowMetaEntry
}

// atomicWrite writes data to path durably: it writes to a sibling temp file
// then renames it over the destination. Rename is atomic on the same
// filesystem, so a crash mid-write can never leave a truncated/corrupt file at
// path — readers see either the old contents or the complete new contents.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, perm); err != nil {
		return err
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp) // best-effort cleanup; don't mask the rename error
		return err
	}
	return nil
}

// safePathSegment reports whether s is safe to use as a single on-disk path
// segment (no separators, no parent-dir escape, non-empty). It mirrors
// service.safeConvComponent so every flowID/scope-keyed path in this backend
// gets the same traversal defense-in-depth as the service-layer convFilePath,
// rather than relying solely on upstream UUID generation + authorization.
func safePathSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	return !strings.ContainsAny(s, `/\`) && !strings.Contains(s, "..")
}

// NewLocalStorageBackend creates a new local file system storage backend
func NewLocalStorageBackend(dataDir string) (*LocalStorageBackend, error) {
	if err := os.MkdirAll(dataDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}

	return &LocalStorageBackend{
		dataDir:      dataDir,
		users:        make(map[string]*interfaces.User),
		usersByEmail: make(map[string]string),
		orgs:         make(map[string]*interfaces.Organisation),
		sharing:      make(map[string][]*interfaces.Collaborator),
	}, nil
}

// flowPath builds the on-disk path for a flow document. Flow IDs can
// originate from uploaded flow files, so they get the same traversal guard
// as every other flowID-keyed path in this backend.
func (lsb *LocalStorageBackend) flowPath(id string) (string, error) {
	if !safePathSegment(id) {
		return "", fmt.Errorf("invalid flow id %q", id)
	}
	return filepath.Join(lsb.dataDir, "flows", id+".json"), nil
}

// SaveFlow saves a flow document to the local file system.
// In local mode OCC is enforced via flowMu + a version comparison.
func (lsb *LocalStorageBackend) SaveFlow(ctx context.Context, flow *interfaces.FlowDocument) error {
	lsb.flowMu.Lock()
	defer lsb.flowMu.Unlock()

	flowPath, err := lsb.flowPath(flow.ID)
	if err != nil {
		return err
	}

	// Check for existing version to enforce OCC. The version comes from the
	// metadata index when fresh (Stat-validated by refreshMetaIndex) — a full
	// read+unmarshal of the stored document under this lock was pure overhead
	// for the common case. A stale/missing index entry falls back to the read.
	existingVersion := -1
	if idx, idxErr := lsb.refreshMetaIndex(ctx); idxErr == nil {
		lsb.metaMu.RLock()
		if e, ok := idx[flow.ID]; ok {
			existingVersion = e.header.Version
		}
		lsb.metaMu.RUnlock()
	}
	if existingVersion == -1 {
		if existing, err := lsb.LoadFlow(ctx, flow.ID); err == nil && existing != nil {
			existingVersion = existing.Version
		}
	}
	if existingVersion >= 0 {
		if flow.Version != existingVersion {
			return interfaces.ErrVersionConflict
		}
		flow.Version = existingVersion + 1
	} else {
		// New flow: force the initial version to match the Postgres backend
		// (INSERT ... VALUES (..., 0)), so a caller passing a stale non-zero
		// Version (e.g. a doc reused across saves) can't fabricate a starting
		// version that would then reject a legitimate Version=0 save.
		flow.Version = 0
	}

	// Create flows directory if it doesn't exist
	if err := os.MkdirAll(filepath.Dir(flowPath), 0750); err != nil {
		return fmt.Errorf("failed to create flows directory: %w", err)
	}

	data, err := json.MarshalIndent(flow, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal flow: %w", err)
	}

	if err := atomicWrite(flowPath, data, 0o600); err != nil {
		return fmt.Errorf("failed to write flow file: %w", err)
	}

	// Keep the metadata index current for this flow so the next listing
	// doesn't re-read the file we just wrote.
	if info, statErr := os.Stat(flowPath); statErr == nil {
		header := *flow
		header.Content = nil
		lsb.metaMu.Lock()
		if lsb.flowMeta == nil {
			lsb.flowMeta = make(map[string]*flowMetaEntry)
		}
		lsb.flowMeta[flow.ID] = &flowMetaEntry{header: header, modTime: info.ModTime(), size: info.Size()}
		lsb.metaMu.Unlock()
	}

	return nil
}

func (lsb *LocalStorageBackend) TransferFlowOwner(_ context.Context, _, _, _ string) error {
	return fmt.Errorf("TransferFlowOwner not supported in local mode")
}

// LoadFlow loads a flow document from the local file system
func (lsb *LocalStorageBackend) LoadFlow(ctx context.Context, id string) (*interfaces.FlowDocument, error) {
	flowPath, err := lsb.flowPath(id)
	if err != nil {
		return nil, err
	}

	data, err := os.ReadFile(flowPath) // #nosec G304 -- flowPath = dataDir/flows/<id>.json with id validated by flowPath
	if err != nil {
		if os.IsNotExist(err) {
			return nil, interfaces.ErrNotFound
		}
		return nil, fmt.Errorf("failed to read flow file: %w", err)
	}

	var flow interfaces.FlowDocument
	if err := json.Unmarshal(data, &flow); err != nil {
		return nil, fmt.Errorf("failed to unmarshal flow: %w", err)
	}

	return &flow, nil
}

// LoadFlowHeader returns the flow's metadata with nil Content. The filesystem
// backend stores content inline, so this just loads and drops it — kept in
// interface parity with the database backend (which skips a blob fetch).
func (lsb *LocalStorageBackend) LoadFlowHeader(ctx context.Context, id string) (*interfaces.FlowDocument, error) {
	flow, err := lsb.LoadFlow(ctx, id)
	if err != nil {
		return nil, err
	}
	flow.Content = nil
	return flow, nil
}

// flowMetaEntry is the cached listing metadata for one stored flow. The index
// lets ListFlows/CountFlows answer from Stat checks (mtime+size) instead of
// reading + fully JSON-parsing every flow file per listing — the defining
// scalability issue of this backend (a 1,000-flow library paid 1,000 reads +
// unmarshals per page render, twice, once more for the count).
type flowMetaEntry struct {
	header  interfaces.FlowDocument // metadata fields only; Content always nil
	modTime time.Time
	size    int64
}

// refreshMetaIndex brings the flow-metadata index up to date: new or changed
// files (Stat mtime/size mismatch) are read once and their HEADER extracted
// (a partial unmarshal — the file is lexed but the content tree is never
// built); unchanged files reuse their cached entry; deleted files drop out.
// Entries for SaveFlow-written files are updated in place by SaveFlow itself.
func (lsb *LocalStorageBackend) refreshMetaIndex(ctx context.Context) (map[string]*flowMetaEntry, error) {
	flowsDir := filepath.Join(lsb.dataDir, "flows")
	files, err := os.ReadDir(flowsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return map[string]*flowMetaEntry{}, nil
		}
		return nil, fmt.Errorf("failed to read flows directory: %w", err)
	}

	lsb.metaMu.Lock()
	defer lsb.metaMu.Unlock()
	if lsb.flowMeta == nil {
		lsb.flowMeta = make(map[string]*flowMetaEntry, len(files))
	}
	seen := make(map[string]bool, len(files))
	for _, file := range files {
		if file.IsDir() || !strings.HasSuffix(file.Name(), ".json") {
			continue
		}
		flowID := file.Name()[:len(file.Name())-5]
		seen[flowID] = true
		info, statErr := file.Info()
		if statErr != nil {
			continue
		}
		if e, ok := lsb.flowMeta[flowID]; ok && e.modTime.Equal(info.ModTime()) && e.size == info.Size() {
			continue // cache hit: one Stat, no read/parse
		}
		data, readErr := os.ReadFile(filepath.Join(flowsDir, file.Name())) // #nosec G304 -- enumerating the backend's own flows dir
		if readErr != nil {
			continue // unreadable: treat as absent (matches previous skip behavior)
		}
		var h interfaces.FlowDocument
		if json.Unmarshal(data, &h) != nil {
			continue // corrupt: skip (matches previous skip behavior)
		}
		h.Content = nil
		lsb.flowMeta[flowID] = &flowMetaEntry{header: h, modTime: info.ModTime(), size: info.Size()}
	}
	for id := range lsb.flowMeta {
		if !seen[id] {
			delete(lsb.flowMeta, id)
		}
	}
	return lsb.flowMeta, nil
}

// ListFlows lists flow documents matching the given filter.
//
// Metadata phase answers from the cached index (Stat-validated); full content
// is loaded ONLY for the flows on the requested page when the caller didn't
// ask for MetadataOnly — previously every listing read and fully parsed every
// stored flow, including all filtered-out ones.
// UpdateFlowTags: tags are a cloud-library concept (organizational labels on
// stored flows); desktop flows are file-backed and carry no tag storage.
// Clean error rather than a silent no-op so callers can branch on mode.
func (lsb *LocalStorageBackend) UpdateFlowTags(ctx context.Context, flowID string, tags []string) error {
	return fmt.Errorf("flow tags require a storage backend (cloud mode)")
}

func (lsb *LocalStorageBackend) ListFlows(ctx context.Context, filter interfaces.FlowFilter) ([]*interfaces.FlowDocument, error) {
	index, err := lsb.refreshMetaIndex(ctx)
	if err != nil {
		return nil, err
	}

	lsb.metaMu.RLock()
	flows := make([]*interfaces.FlowDocument, 0, len(index))
	for id, e := range index {
		doc := e.header // copy; Content nil
		doc.ID = id
		if !lsb.matchesFilter(&doc, filter) {
			continue
		}
		flows = append(flows, &doc)
	}
	lsb.metaMu.RUnlock()

	// Match the Postgres backend's ordering (flowOrderBy) so pagination is
	// stable and the two backends return flows in the same relative order.
	sortFlows(flows, filter.SortBy)

	if filter.Offset > 0 {
		if filter.Offset >= len(flows) {
			return []*interfaces.FlowDocument{}, nil
		}
		flows = flows[filter.Offset:]
	}
	if filter.Limit > 0 && len(flows) > filter.Limit {
		flows = flows[:filter.Limit]
	}

	if filter.MetadataOnly {
		return flows, nil
	}
	// Hydrate content for the page only.
	for _, doc := range flows {
		if full, err := lsb.LoadFlow(ctx, doc.ID); err == nil && full != nil {
			*doc = *full
		}
	}
	return flows, nil
}

// sortFlows orders flows in place to mirror the Postgres flowOrderBy clause, so
// the filesystem backend's pagination is stable and consistent with the
// database backend. Default (unset/unknown sort) = updated_at DESC.
//
// Every comparator ends in a comparison on ID, which is what actually makes the
// claim in the paragraph above true. Two things conspired to break it before:
// ListFlows builds its slice by ranging a MAP (random order per call), and
// slices.SortFunc is NOT a stable sort. So a group of flows comparing equal came
// out in a different arbitrary order on every call.
//
// Ties are the ordinary case here, not a corner one: SaveFlow persists whatever
// UpdatedAt the caller supplied and never stamps one itself, so every flow
// written without an explicit timestamp carries the ZERO time and ties with all
// the others. A bulk import sharing one timestamp does the same.
//
// That matters because ListFlows is walked page-by-page with LIMIT/OFFSET by
// migration.Migrator.migrateFlows and scanner.ScanOnce. Re-ordering between
// pages shifts rows across the boundary, so the walk silently skips some and
// returns others twice — measured at 3 of 40 flows lost with zero-time ties and
// 14 of 40 with a shared timestamp. In the migrator that is silent data loss:
// the run reports the flows it saw and no errors.
//
// ID is unique, so each comparator is now a total order and the sort's own
// stability no longer matters.
func sortFlows(flows []*interfaces.FlowDocument, sort interfaces.FlowSort) {
	byID := func(a, b *interfaces.FlowDocument) int { return strings.Compare(a.ID, b.ID) }
	switch sort {
	case interfaces.FlowSortUpdatedAsc:
		slices.SortFunc(flows, func(a, b *interfaces.FlowDocument) int {
			if c := a.UpdatedAt.Compare(b.UpdatedAt); c != 0 {
				return c
			}
			return byID(a, b)
		})
	case interfaces.FlowSortNameAsc:
		slices.SortFunc(flows, func(a, b *interfaces.FlowDocument) int {
			if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
				return c
			}
			if c := b.UpdatedAt.Compare(a.UpdatedAt); c != 0 {
				return c
			}
			return byID(a, b)
		})
	case interfaces.FlowSortNameDesc:
		slices.SortFunc(flows, func(a, b *interfaces.FlowDocument) int {
			if c := strings.Compare(strings.ToLower(b.Name), strings.ToLower(a.Name)); c != 0 {
				return c
			}
			if c := b.UpdatedAt.Compare(a.UpdatedAt); c != 0 {
				return c
			}
			return byID(a, b)
		})
	case interfaces.FlowSortBlocksDesc:
		// Mirror the Postgres ORDER BY COALESCE((metadata->>'BlockCount')::int, 0)
		// DESC, updated_at DESC. BlockCount lives on FlowMetadata, which is
		// populated even when MetadataOnly is set, so this works for list views.
		slices.SortFunc(flows, func(a, b *interfaces.FlowDocument) int {
			if c := cmp.Compare(b.Metadata.BlockCount, a.Metadata.BlockCount); c != 0 {
				return c
			}
			if c := b.UpdatedAt.Compare(a.UpdatedAt); c != 0 {
				return c
			}
			return byID(a, b)
		})
	case interfaces.FlowSortIDAsc:
		// Immutable sort key — see FlowSortIDAsc's doc comment. Already unique.
		slices.SortFunc(flows, byID)
	default: // FlowSortUpdatedDesc / unset
		slices.SortFunc(flows, func(a, b *interfaces.FlowDocument) int {
			if c := b.UpdatedAt.Compare(a.UpdatedAt); c != 0 {
				return c
			}
			return byID(a, b)
		})
	}
}

// CountFlows returns the total number of flows matching the filter, ignoring
// Limit/Offset. Answers from the same Stat-validated metadata index as
// ListFlows — previously this re-read and re-parsed every stored flow file on
// every call (the library handler calls list AND count per page render).
func (lsb *LocalStorageBackend) CountFlows(ctx context.Context, filter interfaces.FlowFilter) (int, error) {
	index, err := lsb.refreshMetaIndex(ctx)
	if err != nil {
		return 0, err
	}
	lsb.metaMu.RLock()
	defer lsb.metaMu.RUnlock()
	count := 0
	for id, e := range index {
		doc := e.header
		doc.ID = id
		if lsb.matchesFilter(&doc, filter) {
			count++
		}
	}
	return count, nil
}

// matchesFilter returns true if the flow satisfies all set filter conditions.
// Flows with no OwnerID are visible to everyone (backwards compatibility with pre-auth files).
// Note: org filtering here only matches OrganizationID equality — membership of
// UserID in that org is enforced upstream by the service layer (AuthzService)
// before the filter reaches storage.
//
// DELIBERATE DIVERGENCE: collaborator grants are intentionally not checked here.
// The local/desktop backend is single-user; sharing/collaborator logic only
// applies in cloud mode (Postgres). The Postgres flowFilterWhere includes an
// EXISTS subquery on flow_collaborators — this filesystem implementation does not.
func (lsb *LocalStorageBackend) matchesFilter(flow *interfaces.FlowDocument, f interfaces.FlowFilter) bool {
	// AllFlows is an explicit opt-in for operational enumeration (migration)
	// that bypasses owner/org scoping. Otherwise an empty scope matches
	// nothing, mirroring the Postgres backend's defense-in-depth guard.
	if !f.AllFlows {
		if f.UserID == "" && f.OrganizationID == "" && len(f.OrganizationIDs) == 0 {
			return false
		}
		ownerMatch := flow.OwnerID == "" || flow.OwnerID == f.UserID
		orgMatch := f.OrganizationID != "" && flow.OrganizationID == f.OrganizationID
		// OrganizationIDs widens the org scope to multiple orgs the caller
		// belongs to (Postgres uses an IN list); mirror that here so the FS
		// backend doesn't return more rows than Postgres would for the same
		// filter.
		if !orgMatch && len(f.OrganizationIDs) > 0 {
			for _, oid := range f.OrganizationIDs {
				if flow.OrganizationID == oid {
					orgMatch = true
					break
				}
			}
		}
		if !ownerMatch && !orgMatch {
			return false
		}
	}
	if f.Query != "" && !strings.Contains(strings.ToLower(flow.Name), strings.ToLower(f.Query)) {
		return false
	}
	// CreatedAfter / CreatedBefore mirror Postgres's strict > / < comparisons
	// so the two backends agree on which flows a filter returns.
	if f.CreatedAfter != nil && !flow.CreatedAt.After(*f.CreatedAfter) {
		return false
	}
	if f.CreatedBefore != nil && !flow.CreatedAt.Before(*f.CreatedBefore) {
		return false
	}
	return true
}

// DeleteFlow deletes a flow document from the local file system
func (lsb *LocalStorageBackend) DeleteFlow(ctx context.Context, id string) error {
	flowPath, err := lsb.flowPath(id)
	if err != nil {
		return err
	}

	// Take flowMu so a concurrent SaveFlow can't read the file, find it still
	// present, and rewrite it (at a bumped version) after this delete — which
	// would silently resurrect the flow the user just deleted. SaveFlow holds
	// the same mutex across its load-version-check-write critical section.
	lsb.flowMu.Lock()
	defer lsb.flowMu.Unlock()

	if err := os.Remove(flowPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete flow file: %w", err)
	}

	// Drop the metadata index entry so listings immediately stop reporting
	// the deleted flow (no Stat round-trip needed to notice).
	lsb.metaMu.Lock()
	delete(lsb.flowMeta, id)
	lsb.metaMu.Unlock()

	return nil
}

// SaveSettings saves application settings to the local file system
func (lsb *LocalStorageBackend) SaveSettings(ctx context.Context, settings *interfaces.AppSettings) error {
	settingsPath := filepath.Join(lsb.dataDir, "settings.json")

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal settings: %w", err)
	}

	if err := atomicWrite(settingsPath, data, 0o600); err != nil {
		return fmt.Errorf("failed to write settings file: %w", err)
	}

	return nil
}

// LoadSettings loads application settings from the local file system
func (lsb *LocalStorageBackend) LoadSettings(ctx context.Context) (*interfaces.AppSettings, error) {
	settingsPath := filepath.Join(lsb.dataDir, "settings.json")

	data, err := os.ReadFile(settingsPath) // #nosec G304 -- settingsPath is the backend's own dataDir-relative file
	if err != nil {
		if os.IsNotExist(err) {
			// Return default settings if file doesn't exist
			return lsb.getDefaultSettings(), nil
		}
		return nil, fmt.Errorf("failed to read settings file: %w", err)
	}

	var settings interfaces.AppSettings
	if err := json.Unmarshal(data, &settings); err != nil {
		return nil, fmt.Errorf("failed to unmarshal settings: %w", err)
	}

	return &settings, nil
}

// SaveUserSettings saves user-specific settings (redirects to global settings for local mode)
func (lsb *LocalStorageBackend) SaveUserSettings(ctx context.Context, userID string, settings *interfaces.AppSettings) error {
	return lsb.SaveSettings(ctx, settings)
}

// LoadUserSettings loads user-specific settings (redirects to global settings for local mode)
func (lsb *LocalStorageBackend) LoadUserSettings(ctx context.Context, userID string) (*interfaces.AppSettings, error) {
	return lsb.LoadSettings(ctx)
}

// SaveOrgSettings saves org-specific settings (redirects to global settings for local mode)
func (lsb *LocalStorageBackend) SaveOrgSettings(ctx context.Context, orgID string, settings *interfaces.AppSettings) error {
	return lsb.SaveSettings(ctx, settings)
}

// LoadOrgSettings loads org-specific settings (redirects to global settings for local mode)
func (lsb *LocalStorageBackend) LoadOrgSettings(ctx context.Context, orgID string) (*interfaces.AppSettings, error) {
	return lsb.LoadSettings(ctx)
}

// SaveConversation saves a conversation to the local file system
func (lsb *LocalStorageBackend) SaveConversation(ctx context.Context, flowID, scope string, messages []interfaces.ChatMessage) error {
	if !safePathSegment(scope) || !safePathSegment(flowID) {
		return fmt.Errorf("invalid conversation identifier")
	}
	conversationPath := filepath.Join(lsb.dataDir, "conversations", scope, flowID+".json")

	// Create conversations directory if it doesn't exist
	if err := os.MkdirAll(filepath.Dir(conversationPath), 0750); err != nil {
		return fmt.Errorf("failed to create conversations directory: %w", err)
	}

	data, err := json.MarshalIndent(messages, "", "  ")
	if err != nil {
		return fmt.Errorf("failed to marshal conversation: %w", err)
	}

	if err := atomicWrite(conversationPath, data, 0o600); err != nil {
		return fmt.Errorf("failed to write conversation file: %w", err)
	}

	return nil
}

// LoadConversation loads a conversation from the local file system
func (lsb *LocalStorageBackend) LoadConversation(ctx context.Context, flowID, scope string) ([]interfaces.ChatMessage, error) {
	if !safePathSegment(scope) || !safePathSegment(flowID) {
		return nil, fmt.Errorf("invalid conversation identifier")
	}
	conversationPath := filepath.Join(lsb.dataDir, "conversations", scope, flowID+".json")

	data, err := os.ReadFile(conversationPath) // #nosec G304 -- conversationPath is derived from the backend's own dataDir + ids
	if err != nil {
		if os.IsNotExist(err) {
			return []interfaces.ChatMessage{}, nil
		}
		return nil, fmt.Errorf("failed to read conversation file: %w", err)
	}

	var messages []interfaces.ChatMessage
	if err := json.Unmarshal(data, &messages); err != nil {
		return nil, fmt.Errorf("failed to unmarshal conversation: %w", err)
	}

	return messages, nil
}

// DeleteConversation removes the on-disk conversation for a flow+scope. A
// missing file is treated as success so the operation is idempotent.
func (lsb *LocalStorageBackend) DeleteConversation(ctx context.Context, flowID, scope string) error {
	if !safePathSegment(scope) || !safePathSegment(flowID) {
		return fmt.Errorf("invalid conversation identifier")
	}
	conversationPath := filepath.Join(lsb.dataDir, "conversations", scope, flowID+".json")
	if err := os.Remove(conversationPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete conversation file: %w", err)
	}
	return nil
}

// SaveUsageMetric is a no-op: the filesystem backend does not track AI usage
// metrics (local mode has no multi-tenant accounting).
func (b *LocalStorageBackend) SaveUsageMetric(ctx context.Context, metric *interfaces.UsageMetric) error {
	// Local storage does not track usage metrics
	return nil
}

func (b *LocalStorageBackend) GetDailyUsage(ctx context.Context, userID, orgID string) (float64, error) {
	return 0, nil
}

func (b *LocalStorageBackend) SaveKnowledgeDocument(ctx context.Context, doc *interfaces.KnowledgeDocument) error {
	return nil
}
func (b *LocalStorageBackend) DeleteKnowledgeDocument(ctx context.Context, orgID, id string) error {
	return nil
}
func (b *LocalStorageBackend) DeleteKnowledgeDocumentByName(ctx context.Context, orgID, filename string) error {
	return nil
}
func (b *LocalStorageBackend) ListKnowledgeDocuments(ctx context.Context, orgID string) ([]*interfaces.KnowledgeDocument, error) {
	return nil, nil
}
func (b *LocalStorageBackend) SaveKnowledgeChunks(ctx context.Context, userID string, chunks []interfaces.KnowledgeChunk) error {
	return nil
}
func (b *LocalStorageBackend) SearchKnowledge(ctx context.Context, orgID string, queryEmbedding []float32, limit int) ([]interfaces.KnowledgeChunk, error) {
	return nil, nil
}
func (b *LocalStorageBackend) ListKnowledgeChunkContents(ctx context.Context, orgID string) ([]interfaces.KnowledgeChunk, error) {
	return nil, nil
}
func (b *LocalStorageBackend) UpdateKnowledgeChunkEmbeddings(ctx context.Context, userID string, chunks []interfaces.KnowledgeChunk) error {
	return nil
}
func (b *LocalStorageBackend) CountKnowledgeChunks(ctx context.Context, orgID string) (int, int, error) {
	return 0, 0, nil
}

// Audit log — not persisted in local mode.
func (b *LocalStorageBackend) SaveAuditEvent(ctx context.Context, event *interfaces.AuditEvent) error {
	return nil
}
func (b *LocalStorageBackend) ListAuditEvents(ctx context.Context, filter interfaces.AuditFilter) ([]*interfaces.AuditEvent, error) {
	return []*interfaces.AuditEvent{}, nil
}

// Policies — not persisted in local mode (no orgs).
func (b *LocalStorageBackend) SavePolicy(ctx context.Context, p *models.Policy) error {
	return nil
}
func (b *LocalStorageBackend) GetPolicy(ctx context.Context, orgID, id string) (*models.Policy, error) {
	return nil, interfaces.ErrNotFound
}
func (b *LocalStorageBackend) ListPolicies(ctx context.Context, orgID string) ([]*models.Policy, error) {
	return []*models.Policy{}, nil
}
func (b *LocalStorageBackend) DeletePolicy(ctx context.Context, orgID, id string) error {
	return nil
}

// Flow versioning — not supported in local desktop mode.
// versionDir is the per-flow directory holding one JSON file per snapshot:
// {dataDir}/versions/{flowID}/{version}.json. Flow IDs originate from uploads,
// so they get the same traversal guard (safePathSegment) as every other
// flowID-keyed path in this backend.
func (lsb *LocalStorageBackend) versionDir(flowID string) (string, error) {
	if !safePathSegment(flowID) {
		return "", fmt.Errorf("invalid flow id %q", flowID)
	}
	return filepath.Join(lsb.dataDir, "versions", flowID), nil
}

// SaveFlowVersion assigns the next version number atomically (max existing + 1
// under flowMu) and persists the snapshot. Desktop/local equivalent of the
// Postgres FOR UPDATE + MAX+1 path — versioning now works in desktop mode too.
func (lsb *LocalStorageBackend) SaveFlowVersion(ctx context.Context, v *interfaces.FlowVersion) error {
	dir, err := lsb.versionDir(v.FlowID)
	if err != nil {
		return err
	}
	lsb.flowMu.Lock()
	defer lsb.flowMu.Unlock()
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return fmt.Errorf("create versions dir: %w", err)
	}
	// Assign the next version (max existing + 1). The caller's v.Version is
	// ignored — same as the Postgres backend, which computes it atomically.
	existing, _ := os.ReadDir(dir)
	max := 0
	for _, e := range existing {
		var n int
		if _, err := fmt.Sscanf(e.Name(), "%d.json", &n); err == nil && n > max {
			max = n
		}
	}
	v.Version = max + 1
	data, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal version: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%d.json", v.Version)), data, 0o600); err != nil {
		return fmt.Errorf("write version: %w", err)
	}
	return nil
}

// ListFlowVersions returns snapshots newest-first, optionally capped by limit
// (0 = a sensible default to avoid unbounded reads on huge histories).
func (lsb *LocalStorageBackend) ListFlowVersions(ctx context.Context, flowID string, limit int) ([]*interfaces.FlowVersion, error) {
	dir, err := lsb.versionDir(flowID)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return []*interfaces.FlowVersion{}, nil
		}
		return nil, fmt.Errorf("list versions: %w", err)
	}
	var versions []*interfaces.FlowVersion
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(dir, e.Name())) // #nosec G304 -- dir is derived from validated flowID
		if err != nil {
			continue
		}
		var v interfaces.FlowVersion
		if err := json.Unmarshal(data, &v); err != nil {
			continue
		}
		versions = append(versions, &v)
	}
	sort.Slice(versions, func(i, j int) bool { return versions[i].Version > versions[j].Version })
	if limit > 0 && len(versions) > limit {
		versions = versions[:limit]
	}
	return versions, nil
}

// LoadFlowVersion reads a specific snapshot. Returns ErrNotFound when the
// version or flow has no history.
func (lsb *LocalStorageBackend) LoadFlowVersion(ctx context.Context, flowID string, version int) (*interfaces.FlowVersion, error) {
	dir, err := lsb.versionDir(flowID)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(filepath.Join(dir, fmt.Sprintf("%d.json", version))) // #nosec G304 -- dir is derived from validated flowID
	if err != nil {
		if os.IsNotExist(err) {
			return nil, interfaces.ErrNotFound
		}
		return nil, fmt.Errorf("load version: %w", err)
	}
	var v interfaces.FlowVersion
	if err := json.Unmarshal(data, &v); err != nil {
		return nil, fmt.Errorf("unmarshal version: %w", err)
	}
	return &v, nil
}

func (b *LocalStorageBackend) Ping(ctx context.Context) error {
	// Check if data directory is accessible
	if _, err := os.Stat(b.dataDir); err != nil {
		return fmt.Errorf("data directory not accessible: %w", err)
	}
	return nil
}

// Close closes the storage backend
func (lsb *LocalStorageBackend) Close() error {
	// No resources to clean up for local file system
	return nil
}

// getDefaultSettings returns default application settings
func (lsb *LocalStorageBackend) getDefaultSettings() *interfaces.AppSettings {
	return &interfaces.AppSettings{
		Version: 1,
		General: interfaces.GeneralSettings{
			FirstRunCompleted: false,
			LastUsedVersion:   "",
			CheckForUpdates:   "weekly",
		},
		Appearance: interfaces.AppearanceSettings{
			Theme:   "dark",
			Density: "comfortable",
		},
		Layout: interfaces.LayoutSettings{
			SidebarWidth:    280,
			InspectorWidth:  320,
			ChatPanelHeight: nil,
		},
		AI: interfaces.AISettings{
			ActiveProvider: "claude",
			DemoMode: interfaces.DemoModeSettings{
				Enabled:    true,
				DailyLimit: 5,
			},
		},
		Parser: interfaces.ParserSettings{
			MaxFileSizeMB: 50,
		},
	}
}

// ---- User operations ----
//
// Concurrency contract (mirrors apitokens.go): the maps store privately-owned
// structs and every read returns a VALUE COPY, so a caller holding a returned
// user can never race a concurrent UpdateUser* write. Writers store copies so
// a caller mutating its argument after Save can't corrupt the store.

func (lsb *LocalStorageBackend) SaveUser(ctx context.Context, user *interfaces.User) error {
	user.Email = strings.ToLower(user.Email)
	cp := *user
	lsb.mu.Lock()
	defer lsb.mu.Unlock()
	// Reject an email collision with a different user ID to mirror the Postgres
	// UNIQUE(email) constraint (O(1) via the email index).
	if otherID, ok := lsb.usersByEmail[cp.Email]; ok && otherID != cp.ID {
		return interfaces.ErrEmailExists
	}
	if existing, ok := lsb.users[cp.ID]; ok {
		delete(lsb.usersByEmail, existing.Email)
	}
	// Key strictly by ID so each user is stored exactly once.
	lsb.users[cp.ID] = &cp
	lsb.usersByEmail[cp.Email] = cp.ID
	return nil
}

// CreateUser inserts a new user under the users mutex so the empty-check and
// the insert are atomic — two concurrent first-time registrations cannot both
// be promoted to RoleAdmin. Returns ErrEmailExists on email collision.
func (lsb *LocalStorageBackend) CreateUser(ctx context.Context, user *interfaces.User) error {
	user.Email = strings.ToLower(user.Email)
	cp := *user
	lsb.mu.Lock()
	defer lsb.mu.Unlock()

	if _, ok := lsb.usersByEmail[cp.Email]; ok {
		return interfaces.ErrEmailExists
	}

	role := cp.Role
	if len(lsb.users) == 0 && auth.AllowBootstrap(ctx) {
		role = auth.RoleAdmin
	}
	cp.Role = role
	user.Role = role // caller-visible promotion (pre-existing contract)
	lsb.users[cp.ID] = &cp
	lsb.usersByEmail[cp.Email] = cp.ID
	return nil
}

func (lsb *LocalStorageBackend) LoadUserByEmail(ctx context.Context, email string) (*interfaces.User, error) {
	email = strings.ToLower(email)
	lsb.mu.RLock()
	defer lsb.mu.RUnlock()
	if id, ok := lsb.usersByEmail[email]; ok {
		if u, ok := lsb.users[id]; ok {
			cp := *u
			return &cp, nil
		}
	}
	return nil, interfaces.ErrNotFound
}

func (lsb *LocalStorageBackend) LoadUserByID(ctx context.Context, id string) (*interfaces.User, error) {
	lsb.mu.RLock()
	defer lsb.mu.RUnlock()
	if u, ok := lsb.users[id]; ok {
		cp := *u
		return &cp, nil
	}
	return nil, interfaces.ErrNotFound
}

// LoadUsersByIDs resolves multiple users via the in-memory id map (O(len(ids))).
func (lsb *LocalStorageBackend) LoadUsersByIDs(ctx context.Context, ids []string) (map[string]*interfaces.User, error) {
	lsb.mu.RLock()
	defer lsb.mu.RUnlock()
	out := make(map[string]*interfaces.User, len(ids))
	for _, id := range ids {
		if u, ok := lsb.users[id]; ok {
			cp := *u
			out[id] = &cp
		}
	}
	return out, nil
}

func (lsb *LocalStorageBackend) CountUsers(ctx context.Context) (int, error) {
	lsb.mu.RLock()
	defer lsb.mu.RUnlock()
	return len(lsb.users), nil
}

func (lsb *LocalStorageBackend) ListUsers(ctx context.Context, limit, offset int) ([]*interfaces.User, error) {
	lsb.mu.RLock()
	defer lsb.mu.RUnlock()
	users := make([]*interfaces.User, 0, len(lsb.users))
	for _, u := range lsb.users {
		cp := *u
		users = append(users, &cp)
	}
	// Newest-first, tie-broken by ID to match the postgres backend's
	// `ORDER BY created_at DESC, id ASC`.
	//
	// The ID tiebreaker is what makes this actually stable, which the comment
	// here used to claim without delivering: the slice is built by ranging a
	// MAP (random order per call) and sort.Slice is not a stable sort, so any
	// group of users sharing a CreatedAt came out in a different arbitrary
	// order every call — and this list is offset-paginated by the admin UI, so
	// that dropped and repeated users across page boundaries. Ties are easy to
	// hit: a seeded install or a bulk import creates accounts within the same
	// clock tick.
	sort.Slice(users, func(i, j int) bool {
		if !users[i].CreatedAt.Equal(users[j].CreatedAt) {
			return users[i].CreatedAt.After(users[j].CreatedAt)
		}
		return users[i].ID < users[j].ID
	})
	if offset > 0 && offset >= len(users) {
		return users[:0], nil
	}
	if offset > 0 {
		users = users[offset:]
	}
	if limit > 0 && limit < len(users) {
		users = users[:limit]
	}
	return users, nil
}

func (lsb *LocalStorageBackend) ListAdmins(ctx context.Context) ([]*interfaces.User, error) {
	lsb.mu.RLock()
	defer lsb.mu.RUnlock()
	var admins []*interfaces.User
	for _, u := range lsb.users {
		if u.Role == auth.RoleAdmin {
			cp := *u
			admins = append(admins, &cp)
		}
	}
	return admins, nil
}

func (lsb *LocalStorageBackend) UpdateUserRole(ctx context.Context, id string, role auth.Role) error {
	lsb.mu.Lock()
	defer lsb.mu.Unlock()
	if u, ok := lsb.users[id]; ok {
		u.Role = role
		u.UpdatedAt = time.Now().UTC()
		return nil
	}
	return interfaces.ErrNotFound
}

func (lsb *LocalStorageBackend) UpdateUserPassword(ctx context.Context, id string, passwordHash string) error {
	lsb.mu.Lock()
	defer lsb.mu.Unlock()
	if u, ok := lsb.users[id]; ok {
		u.Password = passwordHash
		u.FailedLoginAttempts = 0
		u.LockedUntil = nil
		u.UpdatedAt = time.Now().UTC()
		return nil
	}
	return interfaces.ErrNotFound
}

// DeleteUser erases a local user record. Desktop/local mode is single-user and
// not subject to GDPR multi-tenant erasure semantics, so this only removes the
// in-memory user entry (idempotent). Flow files on disk are left untouched.
func (lsb *LocalStorageBackend) DeleteUser(ctx context.Context, id string) error {
	lsb.mu.Lock()
	defer lsb.mu.Unlock()
	if existing, ok := lsb.users[id]; ok {
		delete(lsb.usersByEmail, existing.Email)
	}
	delete(lsb.users, id)
	return nil
}

// ExportUserData returns a minimal data export for a local user. Local mode has
// no per-user token/audit tables, so only the profile is included.
func (lsb *LocalStorageBackend) ExportUserData(ctx context.Context, id string) (*interfaces.UserDataExport, error) {
	lsb.mu.Lock()
	defer lsb.mu.Unlock()
	u, ok := lsb.users[id]
	if !ok {
		return nil, interfaces.ErrNotFound
	}
	cp := *u
	return &interfaces.UserDataExport{User: &cp, ExportedAt: time.Now().UTC()}, nil
}

// PurgeExpiredData is a no-op in local mode (no expiring token/invite tables).
func (lsb *LocalStorageBackend) PurgeExpiredData(ctx context.Context, auditRetentionDays int) (*interfaces.PurgeResult, error) {
	return &interfaces.PurgeResult{}, nil
}

func (lsb *LocalStorageBackend) UpdateUserProfile(ctx context.Context, id string, displayName, avatarURL string) error {
	lsb.mu.Lock()
	defer lsb.mu.Unlock()
	if u, ok := lsb.users[id]; ok {
		u.DisplayName = displayName
		u.AvatarURL = avatarURL
		u.UpdatedAt = time.Now().UTC()
		return nil
	}
	return interfaces.ErrNotFound
}

// ---- Organisation operations ----
//
// Same copy-on-read/copy-on-write contract as the user store: readers get
// value copies (with the Members slice cloned, since MutateOrg's callback can
// modify elements in place), writers store copies.

func (lsb *LocalStorageBackend) SaveOrg(ctx context.Context, org *interfaces.Organisation) error {
	cp := cloneOrg(org)
	lsb.mu.Lock()
	defer lsb.mu.Unlock()
	lsb.orgs[cp.ID] = &cp
	return nil
}

func (lsb *LocalStorageBackend) LoadOrg(ctx context.Context, id string) (*interfaces.Organisation, error) {
	lsb.mu.RLock()
	defer lsb.mu.RUnlock()
	if o, ok := lsb.orgs[id]; ok {
		cp := cloneOrg(o)
		return &cp, nil
	}
	return nil, interfaces.ErrNotFound
}

func (lsb *LocalStorageBackend) ListOrgsForUser(ctx context.Context, userID string) ([]*interfaces.Organisation, error) {
	lsb.mu.RLock()
	defer lsb.mu.RUnlock()
	var result []*interfaces.Organisation
	for _, o := range lsb.orgs {
		for _, m := range o.Members {
			if m.UserID == userID {
				cp := cloneOrg(o)
				result = append(result, &cp)
				break
			}
		}
	}
	return result, nil
}

func (lsb *LocalStorageBackend) DeleteOrg(ctx context.Context, id string) error {
	lsb.mu.Lock()
	defer lsb.mu.Unlock()
	if _, ok := lsb.orgs[id]; ok {
		delete(lsb.orgs, id)
		return nil
	}
	return interfaces.ErrNotFound
}

// cloneOrg returns a deep-enough copy for the copy-on-read/write contract:
// the struct itself plus a fresh Members backing array (members are values,
// and MutateOrg callbacks can modify them in place).
func cloneOrg(o *interfaces.Organisation) interfaces.Organisation {
	cp := *o
	cp.Members = append([]interfaces.OrgMember(nil), o.Members...)
	return cp
}

// ---- Dashboard ----
// The filesystem backend is not used as the live StorageBackend in desktop mode
// (that path runs with a nil backend and sources the dashboard from the in-memory
// analyzer cache). These satisfy the interface for tests/migration; persistence is
// a no-op and the aggregate is empty.

func (lsb *LocalStorageBackend) SaveFlowAnalysis(ctx context.Context, fa *interfaces.FlowAnalysis) error {
	return nil
}

func (lsb *LocalStorageBackend) LoadFlowHealth(ctx context.Context, flowID string) (*interfaces.HealthSnapshot, error) {
	return nil, nil
}

// LoadFlowHealthBatch: local mode doesn't persist analysis snapshots, so there is
// no health to return — an empty (non-nil) map, matching the cloud contract.
func (lsb *LocalStorageBackend) LoadFlowHealthBatch(ctx context.Context, flowIDs []string) (map[string]*interfaces.HealthSnapshot, error) {
	return map[string]*interfaces.HealthSnapshot{}, nil
}

func (lsb *LocalStorageBackend) FlowDashboardData(ctx context.Context, ownerID string, days int) (*interfaces.DashboardData, error) {
	return &interfaces.DashboardData{ByCategory: map[string]int{}}, nil
}

func (lsb *LocalStorageBackend) FlowDashboardAdvanced(ctx context.Context, ownerID string, days int) (*interfaces.DashboardAdvancedData, error) {
	return &interfaces.DashboardAdvancedData{Security: interfaces.DashboardSecurity{}}, nil
}

// ---- Refresh token operations (local mode stubs) ----

func (lsb *LocalStorageBackend) StoreRefreshToken(ctx context.Context, jti, userID string, expiresAt time.Time) error {
	return nil
}

func (lsb *LocalStorageBackend) IsRefreshTokenValid(ctx context.Context, jti string) (bool, error) {
	return false, nil
}

func (lsb *LocalStorageBackend) RevokeRefreshToken(ctx context.Context, jti string) error {
	return nil
}

func (lsb *LocalStorageBackend) VerifyAndRevokeRefreshToken(ctx context.Context, jti string) (*interfaces.RefreshTokenInfo, error) {
	return nil, interfaces.ErrTokenAlreadyRevoked
}

func (lsb *LocalStorageBackend) RevokeUserRefreshTokens(ctx context.Context, userID string) error {
	return nil
}

func (lsb *LocalStorageBackend) ListUserRefreshTokens(ctx context.Context, userID string) ([]*interfaces.RefreshTokenInfo, error) {
	return nil, nil
}

func (lsb *LocalStorageBackend) RevokeRefreshTokenForUser(ctx context.Context, jti, userID string) error {
	return nil
}

// MutateOrg applies fn to a CLONE of the stored org and, on success, stores
// the clone — the callback's mutations never touch the instance previously
// handed to concurrent readers (copy-on-write, matching the rest of this
// section).
func (lsb *LocalStorageBackend) MutateOrg(ctx context.Context, id string, fn func(*interfaces.Organisation) error) error {
	lsb.mu.Lock()
	defer lsb.mu.Unlock()
	org, ok := lsb.orgs[id]
	if !ok {
		return interfaces.ErrNotFound
	}
	cp := cloneOrg(org)
	if err := fn(&cp); err != nil {
		return err
	}
	lsb.orgs[id] = &cp
	return nil
}

// ---- Sharing operations ----
//
// Copy-on-read/copy-on-write like the user/org stores above: readers get
// fresh slices of value copies; writers store copies.

func (lsb *LocalStorageBackend) ListCollaborators(ctx context.Context, flowID string) ([]*interfaces.Collaborator, error) {
	lsb.mu.RLock()
	defer lsb.mu.RUnlock()
	return cloneCollabs(lsb.sharing[flowID]), nil
}

func (lsb *LocalStorageBackend) ListCollaboratorsBatch(ctx context.Context, flowIDs []string) (map[string][]*interfaces.Collaborator, error) {
	lsb.mu.RLock()
	defer lsb.mu.RUnlock()
	result := make(map[string][]*interfaces.Collaborator, len(flowIDs))
	for _, id := range flowIDs {
		if collabs := lsb.sharing[id]; len(collabs) > 0 {
			result[id] = cloneCollabs(collabs)
		}
	}
	return result, nil
}

// cloneCollabs returns a fresh slice of value copies (nil-safe: an empty,
// non-nil slice for nil input, matching the interface's "empty not nil"
// convention).
func cloneCollabs(in []*interfaces.Collaborator) []*interfaces.Collaborator {
	out := make([]*interfaces.Collaborator, len(in))
	for i, c := range in {
		cp := *c
		out[i] = &cp
	}
	return out
}

func (lsb *LocalStorageBackend) AddCollaborator(ctx context.Context, flowID string, c *interfaces.Collaborator) error {
	cp := *c
	if cp.GrantedAt.IsZero() {
		cp.GrantedAt = time.Now().UTC()
	}
	lsb.mu.Lock()
	defer lsb.mu.Unlock()
	list := lsb.sharing[flowID]
	for i, existing := range list {
		if existing.UserID == cp.UserID {
			list[i] = &cp
			return nil
		}
	}
	lsb.sharing[flowID] = append(list, &cp)
	return nil
}

func (lsb *LocalStorageBackend) UpdateCollaborator(ctx context.Context, flowID, userID string, permission string) error {
	lsb.mu.Lock()
	defer lsb.mu.Unlock()
	list := lsb.sharing[flowID]
	for i, existing := range list {
		if existing.UserID == userID {
			cp := *existing
			cp.Permission = permission
			list[i] = &cp
			return nil
		}
	}
	return interfaces.ErrNotFound
}

func (lsb *LocalStorageBackend) RemoveCollaborator(ctx context.Context, flowID, userID string) error {
	lsb.mu.Lock()
	defer lsb.mu.Unlock()
	list := lsb.sharing[flowID]
	for i, existing := range list {
		if existing.UserID == userID {
			lsb.sharing[flowID] = append(list[:i], list[i+1:]...)
			return nil
		}
	}
	return interfaces.ErrNotFound
}

// Org custom rules are a cloud governance feature — desktop has no orgs. The
// single-user path keeps using the deployment-level rule file
// (PAD_CUSTOM_RULES), which the resolver falls back to when there is no org.
//
// Save/Delete report the mode mismatch rather than silently succeeding; List
// returns empty (not an error) because the ANALYSIS path calls it on every run
// and must degrade to "no org rules", not fail the analysis.
func (b *LocalStorageBackend) SaveOrgCustomRule(ctx context.Context, rule *interfaces.OrgCustomRule) error {
	return fmt.Errorf("org custom rules require a storage backend (cloud mode)")
}
func (b *LocalStorageBackend) DeleteOrgCustomRule(ctx context.Context, orgID, id string) error {
	return fmt.Errorf("org custom rules require a storage backend (cloud mode)")
}
func (b *LocalStorageBackend) ListOrgCustomRules(ctx context.Context, orgID string, enabledOnly bool) ([]*interfaces.OrgCustomRule, error) {
	return nil, nil
}

// Org channels are a cloud-library governance feature (per-org routing of
// external notifications); desktop has no orgs to route for.
func (b *LocalStorageBackend) SaveOrgChannel(ctx context.Context, ch *interfaces.OrgChannel) error {
	return fmt.Errorf("org channels require a storage backend (cloud mode)")
}
func (b *LocalStorageBackend) DeleteOrgChannel(ctx context.Context, orgID, id string) error {
	return fmt.Errorf("org channels require a storage backend (cloud mode)")
}
func (b *LocalStorageBackend) ListOrgChannels(ctx context.Context, orgID string, enabledOnly bool) ([]*interfaces.OrgChannel, error) {
	return nil, nil
}

// SearchFlowContents: the filesystem backend has no queryable content store
// (flows are parsed files); the service falls back to its legacy scan.
func (b *LocalStorageBackend) SearchFlowContents(ctx context.Context, filter interfaces.FlowFilter, needle string, limit int) ([]*interfaces.FlowDocument, error) {
	return nil, interfaces.ErrContentSearchUnsupported
}
