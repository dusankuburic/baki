package filesystem

import (
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/atomicfile"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// LocalStorageBackend implements StorageBackend on the local file system — NOT the desktop app's store
// (local mode injects no StorageBackend at all). Live uses: the legacy-server migration source and the API test router.
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
	// flowMeta caches listing metadata per flow ID (see flowMetaEntry),
	// guarded by metaMu; invalidated by Stat (mtime+size) on refresh.
	metaMu   sync.RWMutex
	flowMeta map[string]*flowMetaEntry
}

// atomicWrite writes data to path durably via a unique sibling temp file + rename (atomic — a crash mid-write
// can never leave a truncated file). The temp name must be unique per call: a fixed path+".tmp" let concurrent
// writers interleave O_TRUNC writes and publish a mixed body; the dot-prefix keeps orphaned temps out of .json filters.
func atomicWrite(path string, data []byte, perm os.FileMode) error {
	tmp, err := os.CreateTemp(filepath.Dir(path), ".baki-tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) } // best-effort; don't mask the real error

	// CreateTemp creates with 0600; honor the caller's requested mode
	// (governance alerts are 0644, everything else 0600).
	if err := tmp.Chmod(perm); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := renameOverOpenReader(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// renameOverOpenReader renames oldpath over newpath, tolerating the one way Windows departs from POSIX
// rename semantics: replacing a file another goroutine momentarily holds open for reading (a LoadFlow racing
// a Save) fails with a sharing violation instead of atomically swapping — the handle closes within
// microseconds, so a short bounded retry succeeds. On non-Windows, rename is atomic over open files and
// EPERM is a genuine permission problem — no retry. Impl: internal/storage/atomicfile.
func renameOverOpenReader(oldpath, newpath string) error {
	return atomicfile.RenameOverOpenReader(oldpath, newpath)
}

// readFileTolerant is the read-side mirror of renameOverOpenReader: a concurrent OPEN while the writer's
// rename replaces the destination fails with the same transient sharing violation, so the same bounded retry applies.
func readFileTolerant(path string) ([]byte, error) {
	return atomicfile.ReadFileTolerant(path)
}

// safePathSegment reports whether s is safe as a single on-disk path segment (no separators, no
// parent-dir escape, non-empty); mirrors service.safeConvComponent for traversal defense-in-depth.
func safePathSegment(s string) bool {
	if s == "" || s == "." || s == ".." {
		return false
	}
	return !strings.ContainsAny(s, `/\`) && !strings.Contains(s, "..")
}

// staleTempMaxAge bounds how old a ".baki-tmp-*" file must be before the startup sweep considers it
// crash-orphaned: atomicWrite removes its temp on every failure path, so only a process death leaves one.
const staleTempMaxAge = 10 * time.Minute

// sweepStaleTemps removes crash-orphaned atomicWrite temps under root, to a bounded depth (3 covers
// every dir atomicWrite writes into). Best-effort: errors are logged, never fatal.
func sweepStaleTemps(root string) {
	const maxDepth = 3
	cutoff := time.Now().Add(-staleTempMaxAge)
	var walk func(dir string, depth int)
	walk = func(dir string, depth int) {
		if depth > maxDepth {
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			slog.Warn("local storage: temp sweep cannot read directory", "dir", dir, "error", err)
			return
		}
		for _, e := range entries {
			name := e.Name()
			if e.IsDir() {
				walk(filepath.Join(dir, name), depth+1)
				continue
			}
			if !strings.HasPrefix(name, ".baki-tmp-") {
				continue
			}
			full := filepath.Join(dir, name)
			info, err := e.Info()
			if err != nil {
				slog.Warn("local storage: temp sweep cannot stat file", "file", full, "error", err)
				continue
			}
			if info.ModTime().After(cutoff) {
				continue // young enough to be a live writer's temp
			}
			if err := os.Remove(full); err != nil {
				slog.Warn("local storage: temp sweep cannot remove crash-orphaned temp", "file", full, "error", err)
			} else {
				slog.Info("local storage: removed crash-orphaned temp file", "file", full, "age", time.Since(info.ModTime()).Round(time.Second))
			}
		}
	}
	walk(root, 0)
}

// NewLocalStorageBackend creates a new local file system storage backend
func NewLocalStorageBackend(dataDir string) (*LocalStorageBackend, error) {
	if err := os.MkdirAll(dataDir, 0750); err != nil {
		return nil, fmt.Errorf("failed to create data directory: %w", err)
	}
	sweepStaleTemps(dataDir)

	return &LocalStorageBackend{
		dataDir:      dataDir,
		users:        make(map[string]*interfaces.User),
		usersByEmail: make(map[string]string),
		orgs:         make(map[string]*interfaces.Organisation),
		sharing:      make(map[string][]*interfaces.Collaborator),
	}, nil
}

// flowPath builds the on-disk path for a flow document; IDs can originate from uploaded files, so
// they get the same traversal guard as every other flowID-keyed path in this backend.
func (lsb *LocalStorageBackend) flowPath(id string) (string, error) {
	if !safePathSegment(id) {
		return "", fmt.Errorf("invalid flow id %q", id)
	}
	return filepath.Join(lsb.dataDir, "flows", id+".json"), nil
}

// SaveFlow saves a flow document; OCC is enforced via flowMu + a version comparison.
func (lsb *LocalStorageBackend) SaveFlow(ctx context.Context, flow *interfaces.FlowDocument) error {
	lsb.flowMu.Lock()
	defer lsb.flowMu.Unlock()

	flowPath, err := lsb.flowPath(flow.ID)
	if err != nil {
		return err
	}

	// OCC version check: from the metadata index when fresh (Stat-validated), else a full read.
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
		// Force the initial version to 0 like Postgres, so a stale non-zero Version cannot
		// fabricate a starting version that rejects a legitimate Version=0 save.
		flow.Version = 0
	}

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

	// Keep the index current so the next listing doesn't re-read the file we just wrote.
	if info, statErr := os.Stat(flowPath); statErr == nil {
		header := *flow
		header.Content = nil
		header.Source = "" // see flowMetaEntry: the index must not hold bodies
		lsb.metaMu.Lock()
		if lsb.flowMeta == nil {
			lsb.flowMeta = make(map[string]*flowMetaEntry)
		}
		lsb.flowMeta[flow.ID] = &flowMetaEntry{header: header, modTime: info.ModTime(), size: info.Size()}
		lsb.metaMu.Unlock()
	}

	return nil
}

// LoadFlow loads a flow document from the local file system
func (lsb *LocalStorageBackend) LoadFlow(ctx context.Context, id string) (*interfaces.FlowDocument, error) {
	flowPath, err := lsb.flowPath(id)
	if err != nil {
		return nil, err
	}

	data, err := readFileTolerant(flowPath) // #nosec G304 -- flowPath = dataDir/flows/<id>.json with id validated by flowPath
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

// LoadFlowHeader returns the flow's metadata with nil Content and empty Source (interface parity;
// no header caller may depend on either).
func (lsb *LocalStorageBackend) LoadFlowHeader(ctx context.Context, id string) (*interfaces.FlowDocument, error) {
	flow, err := lsb.LoadFlow(ctx, id)
	if err != nil {
		return nil, err
	}
	flow.Content = nil
	flow.Source = ""
	return flow, nil
}

// flowMetaEntry is the cached listing metadata for one stored flow, letting ListFlows/CountFlows answer
// from Stat checks instead of reading + parsing every flow file. The header holds neither body: the index
// lives as long as the backend, so a kept Source would pin every legacy flow's full PAD text for the process lifetime.
type flowMetaEntry struct {
	header  interfaces.FlowDocument // metadata fields only; Content nil, Source empty
	modTime time.Time
	size    int64
}

// refreshMetaIndex brings the flow-metadata index up to date: new/changed files (Stat mtime/size
// mismatch) are read once and their HEADER extracted; unchanged reuse the cached entry; deleted drop out.
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
		data, readErr := readFileTolerant(filepath.Join(flowsDir, file.Name())) // #nosec G304 -- enumerating the backend's own flows dir
		if readErr != nil {
			continue // unreadable: treat as absent (matches previous skip behavior)
		}
		var h interfaces.FlowDocument
		if json.Unmarshal(data, &h) != nil {
			continue // corrupt: skip (matches previous skip behavior)
		}
		h.Content = nil
		h.Source = "" // see flowMetaEntry: the index must not hold bodies
		lsb.flowMeta[flowID] = &flowMetaEntry{header: h, modTime: info.ModTime(), size: info.Size()}
	}
	for id := range lsb.flowMeta {
		if !seen[id] {
			delete(lsb.flowMeta, id)
		}
	}
	return lsb.flowMeta, nil
}

// UpdateFlowTags: tags are a cloud-library concept; desktop flows are file-backed. Clean error
// rather than a silent no-op so callers can branch on mode.
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
		doc := e.header // copy; Content nil, Source empty
		doc.ID = id
		if !lsb.matchesFilter(&doc, filter) {
			continue
		}
		flows = append(flows, &doc)
	}
	lsb.metaMu.RUnlock()

	// Match the Postgres backend's ordering (flowOrderBy) so pagination is stable across backends.
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

// sortFlows orders flows in place to mirror the Postgres flowOrderBy clause, so pagination is stable
// across backends. Every comparator ends in an ID comparison: ListFlows builds its slice by ranging a
// MAP and slices.SortFunc is NOT stable, so without a unique tiebreaker the LIMIT/OFFSET walkers
// silently skip and repeat rows (measured: 3 of 40 flows lost).
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
		// Mirror the Postgres ORDER BY COALESCE(BlockCount,0) DESC, updated_at DESC.
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

// CountFlows returns the total matching the filter, ignoring Limit/Offset, from the Stat-validated
// metadata index.
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

// matchesFilter returns true if the flow satisfies all set filter conditions. Flows with no OwnerID
// are visible to everyone (pre-auth compatibility); org membership of UserID is enforced upstream.
// DELIBERATE DIVERGENCE: collaborator grants are not checked — the local/desktop backend is
// single-user, sharing applies only in cloud mode.
func (lsb *LocalStorageBackend) matchesFilter(flow *interfaces.FlowDocument, f interfaces.FlowFilter) bool {
	// AllFlows is the operational-enumeration opt-in (migration) that bypasses owner/org scoping;
	// otherwise an empty scope matches nothing, mirroring the Postgres defense-in-depth guard.
	if !f.AllFlows {
		if f.UserID == "" && f.OrganizationID == "" && len(f.OrganizationIDs) == 0 {
			return false
		}
		ownerMatch := flow.OwnerID == "" || flow.OwnerID == f.UserID
		orgMatch := f.OrganizationID != "" && flow.OrganizationID == f.OrganizationID
		// OrganizationIDs widens the org scope (Postgres uses an IN list); mirror it so the FS
		// backend doesn't return more rows than Postgres would.
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
	// CreatedAfter / CreatedBefore mirror Postgres's strict > / < comparisons.
	if f.CreatedAfter != nil && !flow.CreatedAt.After(*f.CreatedAfter) {
		return false
	}
	if f.CreatedBefore != nil && !flow.CreatedAt.Before(*f.CreatedBefore) {
		return false
	}
	// Keyset cursor (FlowFilter.AfterID), mirroring Postgres's id > $n.
	if f.AfterID != "" && flow.ID <= f.AfterID {
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

	// Take flowMu so a concurrent SaveFlow can't find the file still present and rewrite it after
	// this delete — silently resurrecting the flow the user just deleted.
	lsb.flowMu.Lock()
	defer lsb.flowMu.Unlock()

	if err := os.Remove(flowPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to delete flow file: %w", err)
	}

	// Drop the index entry so listings immediately stop reporting the deleted flow.
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

	data, err := readFileTolerant(settingsPath) // #nosec G304 -- settingsPath is the backend's own dataDir-relative file
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

	data, err := readFileTolerant(conversationPath) // #nosec G304 -- conversationPath is derived from the backend's own dataDir + ids
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

// DeleteConversation removes the on-disk conversation for a flow+scope; a missing file is success.
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

// SaveUsageMetric is a no-op: the filesystem backend tracks no AI usage metrics.
func (b *LocalStorageBackend) SaveUsageMetric(ctx context.Context, metric *interfaces.UsageMetric) error {
	return nil
}

func (b *LocalStorageBackend) GetDailyUsage(ctx context.Context, userID, orgID string) (float64, error) {
	return 0, nil
}

func (b *LocalStorageBackend) SaveKnowledgeDocument(ctx context.Context, _ string, doc *interfaces.KnowledgeDocument) error {
	return nil
}
func (b *LocalStorageBackend) DeleteKnowledgeDocument(ctx context.Context, _, orgID, id string) error {
	return nil
}
func (b *LocalStorageBackend) DeleteKnowledgeDocumentByName(ctx context.Context, orgID, filename string) error {
	return nil
}
func (b *LocalStorageBackend) ListKnowledgeDocuments(ctx context.Context, orgID string, limit, offset int) ([]*interfaces.KnowledgeDocument, error) {
	return nil, nil
}
func (b *LocalStorageBackend) ListKnowledgeDocumentIDsByFilename(ctx context.Context, orgID, filename string) ([]string, int, error) {
	return nil, 0, nil
}
func (b *LocalStorageBackend) SaveKnowledgeChunks(ctx context.Context, userID string, chunks []interfaces.KnowledgeChunk) error {
	return nil
}
func (b *LocalStorageBackend) SearchKnowledge(ctx context.Context, orgID string, queryEmbedding []float32, limit int) ([]interfaces.KnowledgeChunk, error) {
	return nil, nil
}
func (b *LocalStorageBackend) ListKnowledgeChunkContents(ctx context.Context, orgID string, after interfaces.KnowledgeChunkCursor, limit int) ([]interfaces.KnowledgeChunk, error) {
	return nil, nil
}
func (b *LocalStorageBackend) SampleKnowledgeChunkEmbeddings(ctx context.Context, orgID string, n int) ([]interfaces.KnowledgeChunk, error) {
	return nil, nil
}
func (b *LocalStorageBackend) SampleKnowledgeChunkEmbeddingsUpTo(ctx context.Context, orgID string, upTo interfaces.KnowledgeChunkCursor, n int) ([]interfaces.KnowledgeChunk, error) {
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

// ErrPoliciesUnsupported is returned by the policy methods that would otherwise have to invent a
// result: named policies are an org-scoped cloud-mode feature; this backend has no orgs.
var ErrPoliciesUnsupported = errors.New("named policies require cloud mode (this deployment stores flows locally)")

// Policies — not persisted in local mode (no orgs). Save/Delete return an error: a silent success is
// worse (a user saved a policy, was told it worked, and it was gone); Get/List keep returning "nothing here".
func (b *LocalStorageBackend) SavePolicy(ctx context.Context, p *models.Policy) error {
	return ErrPoliciesUnsupported
}
func (b *LocalStorageBackend) GetPolicy(ctx context.Context, orgID, id string) (*models.Policy, error) {
	return nil, interfaces.ErrNotFound
}
func (b *LocalStorageBackend) ListPolicies(ctx context.Context, orgID string) ([]*models.Policy, error) {
	return []*models.Policy{}, nil
}
func (b *LocalStorageBackend) DeletePolicy(ctx context.Context, orgID, id string) error {
	return ErrPoliciesUnsupported
}

// Flow versioning (test router / legacy data only). versionDir is the per-flow directory holding
// one JSON per snapshot: {dataDir}/versions/{flowID}/{version}.json (traversal-guarded).
func (lsb *LocalStorageBackend) versionDir(flowID string) (string, error) {
	if !safePathSegment(flowID) {
		return "", fmt.Errorf("invalid flow id %q", flowID)
	}
	return filepath.Join(lsb.dataDir, "versions", flowID), nil
}

// SaveFlowVersion assigns the next version atomically (max existing + 1 under flowMu) and persists
// the snapshot — the FS equivalent of Postgres's FOR UPDATE + MAX+1.
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
	// The caller's v.Version is ignored — same as Postgres, which computes it atomically.
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

// ListFlowVersions returns snapshots newest-first, optionally capped by limit (0 = a sensible default).
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
		data, err := readFileTolerant(filepath.Join(dir, e.Name())) // #nosec G304 -- dir is derived from validated flowID
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

// LoadFlowVersion reads one snapshot; ErrNotFound when the version or flow has no history.
func (lsb *LocalStorageBackend) LoadFlowVersion(ctx context.Context, flowID string, version int) (*interfaces.FlowVersion, error) {
	dir, err := lsb.versionDir(flowID)
	if err != nil {
		return nil, err
	}
	data, err := readFileTolerant(filepath.Join(dir, fmt.Sprintf("%d.json", version))) // #nosec G304 -- dir is derived from validated flowID
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
		},
		Appearance: interfaces.AppearanceSettings{
			Theme:        "dark",
			Density:      "comfortable",
			ChatTextSize: "md",
		},
		Layout: interfaces.LayoutSettings{
			SidebarWidth:   280,
			InspectorWidth: 320,
		},
		AI: interfaces.AISettings{
			ActiveProvider: "claude",
		},
		Parser: interfaces.ParserSettings{
			MaxFileSizeMB: 50,
		},
	}
}

// ---- User operations ----
// Concurrency contract: the maps store privately-owned structs and every read returns a VALUE COPY,
// so a caller holding a returned user can never race a concurrent UpdateUser* write.

func (lsb *LocalStorageBackend) SaveUser(ctx context.Context, user *interfaces.User) error {
	user.Email = strings.ToLower(user.Email)
	cp := *user
	lsb.mu.Lock()
	defer lsb.mu.Unlock()
	// Mirror the Postgres UNIQUE(email) constraint (O(1) via the email index).
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

// CreateUser inserts under the users mutex so the empty-check and insert are atomic (two
// concurrent first-time registrations cannot both become RoleAdmin). ErrEmailExists on collision.
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
		// Bootstrap admin is email-verified so a deployment without SMTP can sign in its first admin.
		cp.EmailVerified = true
		user.EmailVerified = true
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
	// Newest-first, tie-broken by ID to match Postgres `ORDER BY created_at DESC, id ASC`: the slice is
	// built by ranging a MAP and sort.Slice is not stable — without the tiebreaker offset pagination
	// dropped and repeated users across page boundaries.
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

func (lsb *LocalStorageBackend) QueryUsers(ctx context.Context, q interfaces.UserQuery) ([]*interfaces.User, int, error) {
	// ListUsers already hands out copies in the shared order.
	all, err := lsb.ListUsers(ctx, 0, 0)
	if err != nil {
		return nil, 0, err
	}
	page, total := q.Apply(all)
	return page, total, nil
}

func (lsb *LocalStorageBackend) LoadUserStats(ctx context.Context) (*interfaces.UserStats, error) {
	lsb.mu.RLock()
	defer lsb.mu.RUnlock()
	users := make([]*interfaces.User, 0, len(lsb.users))
	for _, u := range lsb.users {
		users = append(users, u)
	}
	return interfaces.UserStatsOf(users, time.Now().UTC()), nil
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

// DeleteUser erases a local user record (in-memory only, idempotent); local mode is single-user,
// not subject to GDPR erasure. Flow files on disk are left untouched.
func (lsb *LocalStorageBackend) DeleteUser(ctx context.Context, id string) error {
	lsb.mu.Lock()
	defer lsb.mu.Unlock()
	if existing, ok := lsb.users[id]; ok {
		delete(lsb.usersByEmail, existing.Email)
	}
	delete(lsb.users, id)
	return nil
}

// ExportUserData returns a minimal export (profile only) — local mode has no per-user token/audit tables.
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
// Same copy-on-read/copy-on-write contract as the user store (Members slice cloned — MutateOrg's
// callback can modify elements in place).

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

// cloneOrg returns a deep-enough copy for the copy-on-read/write contract: struct plus a fresh
// Members backing array.
func cloneOrg(o *interfaces.Organisation) interfaces.Organisation {
	cp := *o
	cp.Members = append([]interfaces.OrgMember(nil), o.Members...)
	return cp
}

// ---- Dashboard ----
// Not the live StorageBackend in desktop mode (that path runs with a nil backend); these satisfy
// the interface for tests/migration — persistence is a no-op and the aggregate is empty.

func (lsb *LocalStorageBackend) SaveFlowAnalysis(ctx context.Context, fa *interfaces.FlowAnalysis) error {
	return nil
}

func (lsb *LocalStorageBackend) LoadFlowHealth(ctx context.Context, flowID string) (*interfaces.HealthSnapshot, error) {
	return nil, nil
}

// LoadFlowHealthBatch: local mode persists no snapshots — an empty (non-nil) map, per the cloud contract.
func (lsb *LocalStorageBackend) LoadFlowHealthBatch(ctx context.Context, flowIDs []string) (map[string]*interfaces.HealthSnapshot, error) {
	return map[string]*interfaces.HealthSnapshot{}, nil
}

func (lsb *LocalStorageBackend) FlowDashboardData(ctx context.Context, ownerID string, days int) (*interfaces.DashboardData, error) {
	return &interfaces.DashboardData{ByCategory: map[string]int{}}, nil
}

func (lsb *LocalStorageBackend) FlowDashboardAdvanced(ctx context.Context, ownerID string, days int) (*interfaces.DashboardAdvancedData, error) {
	return &interfaces.DashboardAdvancedData{Security: interfaces.DashboardSecurity{}}, nil
}

// Analytics reads: local mode persists no analysis history, so every read is empty.
func (lsb *LocalStorageBackend) LoadAnalysisSnapshots(ctx context.Context, flowIDs []string) ([]interfaces.AnalysisSnapshotRow, error) {
	return []interfaces.AnalysisSnapshotRow{}, nil
}

func (lsb *LocalStorageBackend) LoadAnalysisHistory(ctx context.Context, w interfaces.AnalyticsWindow) (*interfaces.AnalysisHistoryRows, error) {
	return &interfaces.AnalysisHistoryRows{}, nil
}

func (lsb *LocalStorageBackend) LoadTriageAnalytics(ctx context.Context, w interfaces.AnalyticsWindow) (*interfaces.TriageRows, error) {
	return &interfaces.TriageRows{}, nil
}

func (lsb *LocalStorageBackend) LoadUsageAnalytics(ctx context.Context, userID string, orgIDs []string, mode string, w interfaces.AnalyticsWindow) (*interfaces.UsageRows, error) {
	return &interfaces.UsageRows{}, nil
}

func (lsb *LocalStorageBackend) LoadActivityAnalytics(ctx context.Context, userID string, w interfaces.AnalyticsWindow, limit int) (*interfaces.ActivityRows, error) {
	return &interfaces.ActivityRows{}, nil
}

func (lsb *LocalStorageBackend) LoadSecurityAnalytics(ctx context.Context, userID, email string, includeLocked bool) (*interfaces.SecurityRows, error) {
	return &interfaces.SecurityRows{}, nil
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

// MutateOrg applies fn to a CLONE and stores the clone on success — the callback's mutations never
// touch the instance previously handed to concurrent readers.
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
// Copy-on-read/copy-on-write like the user/org stores above.

func (lsb *LocalStorageBackend) ListCollaborators(ctx context.Context, flowID string, limit int) ([]*interfaces.Collaborator, error) {
	lsb.mu.RLock()
	defer lsb.mu.RUnlock()
	out := cloneCollabs(lsb.sharing[flowID])
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
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

// cloneCollabs returns a fresh slice of value copies (nil-safe: empty non-nil for nil input).
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

// Org custom rules are a cloud governance feature — desktop has no orgs (the single-user path keeps the
// deployment-level PAD_CUSTOM_RULES file). Save/Delete report the mode mismatch; List returns empty
// because the ANALYSIS path calls it on every run and must degrade to "no org rules", not fail.
func (b *LocalStorageBackend) SaveOrgCustomRule(ctx context.Context, rule *interfaces.OrgCustomRule) error {
	return fmt.Errorf("org custom rules require a storage backend (cloud mode)")
}
func (b *LocalStorageBackend) DeleteOrgCustomRule(ctx context.Context, orgID, id string) error {
	return fmt.Errorf("org custom rules require a storage backend (cloud mode)")
}
func (b *LocalStorageBackend) ListOrgCustomRules(ctx context.Context, orgID string, enabledOnly bool) ([]*interfaces.OrgCustomRule, error) {
	return nil, nil
}

// Org channels are a cloud-library governance feature; desktop has no orgs to route for.
func (b *LocalStorageBackend) SaveOrgChannel(ctx context.Context, ch *interfaces.OrgChannel) error {
	return fmt.Errorf("org channels require a storage backend (cloud mode)")
}
func (b *LocalStorageBackend) DeleteOrgChannel(ctx context.Context, orgID, id string) error {
	return fmt.Errorf("org channels require a storage backend (cloud mode)")
}
func (b *LocalStorageBackend) ListOrgChannels(ctx context.Context, orgID string, enabledOnly bool) ([]*interfaces.OrgChannel, error) {
	return nil, nil
}

// SearchFlowContents: the filesystem backend has no queryable content store; the service falls
// back to its legacy scan.
func (b *LocalStorageBackend) SearchFlowContents(ctx context.Context, filter interfaces.FlowFilter, needle string, limit int) ([]*interfaces.FlowDocument, error) {
	return nil, interfaces.ErrContentSearchUnsupported
}
