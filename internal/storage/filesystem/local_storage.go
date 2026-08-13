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
	orgs         map[string]*interfaces.Organisation
	sharing      map[string][]*interfaces.Collaborator
	apiTokens    map[string]*interfaces.APIToken  // guarded by apiTokenMu
	userTokens   map[string]*interfaces.UserToken // keyed by token hash; guarded by userTokenMu
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
		dataDir: dataDir,
		users:   make(map[string]*interfaces.User),
		orgs:    make(map[string]*interfaces.Organisation),
		sharing: make(map[string][]*interfaces.Collaborator),
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

	// Check for existing version to enforce OCC
	if existing, err := lsb.LoadFlow(ctx, flow.ID); err == nil && existing != nil {
		if flow.Version != existing.Version {
			return interfaces.ErrVersionConflict
		}
		flow.Version = existing.Version + 1
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

// ListFlows lists flow documents matching the given filter
func (lsb *LocalStorageBackend) ListFlows(ctx context.Context, filter interfaces.FlowFilter) ([]*interfaces.FlowDocument, error) {
	flowsDir := filepath.Join(lsb.dataDir, "flows")

	// Check if flows directory exists
	if _, err := os.Stat(flowsDir); os.IsNotExist(err) {
		return []*interfaces.FlowDocument{}, nil
	}

	files, err := os.ReadDir(flowsDir)
	if err != nil {
		return nil, fmt.Errorf("failed to read flows directory: %w", err)
	}

	var flows []*interfaces.FlowDocument
	for _, file := range files {
		if file.IsDir() {
			continue
		}

		// Skip non-.json files (temp files, backups, etc.) to avoid
		// index-out-of-range panics on the extension strip below.
		if !strings.HasSuffix(file.Name(), ".json") {
			continue
		}

		// Extract flow ID from filename (strip ".json", 5 chars)
		flowID := file.Name()[:len(file.Name())-5]

		flow, err := lsb.LoadFlow(ctx, flowID)
		if err != nil {
			// Skip files that can't be loaded
			continue
		}

		if !lsb.matchesFilter(flow, filter) {
			continue
		}

		if filter.MetadataOnly {
			flow.Content = nil // list view needs metadata only
		}
		flows = append(flows, flow)
	}

	// Match the Postgres backend's ordering (flowOrderBy) so pagination is
	// stable and the two backends return flows in the same relative order.
	// os.ReadDir yields alphabetical-by-filename order, which is unrelated to
	// updated_at and would make page-by-page iteration skip/duplicate flows.
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

	return flows, nil
}

// sortFlows orders flows in place to mirror the Postgres flowOrderBy clause, so
// the filesystem backend's pagination is stable and consistent with the
// database backend. Default (unset/unknown sort) = updated_at DESC.
func sortFlows(flows []*interfaces.FlowDocument, sort interfaces.FlowSort) {
	switch sort {
	case interfaces.FlowSortUpdatedAsc:
		slices.SortFunc(flows, func(a, b *interfaces.FlowDocument) int {
			return a.UpdatedAt.Compare(b.UpdatedAt)
		})
	case interfaces.FlowSortNameAsc:
		slices.SortFunc(flows, func(a, b *interfaces.FlowDocument) int {
			if c := strings.Compare(strings.ToLower(a.Name), strings.ToLower(b.Name)); c != 0 {
				return c
			}
			return b.UpdatedAt.Compare(a.UpdatedAt)
		})
	case interfaces.FlowSortNameDesc:
		slices.SortFunc(flows, func(a, b *interfaces.FlowDocument) int {
			if c := strings.Compare(strings.ToLower(b.Name), strings.ToLower(a.Name)); c != 0 {
				return c
			}
			return b.UpdatedAt.Compare(a.UpdatedAt)
		})
	case interfaces.FlowSortBlocksDesc:
		// Mirror the Postgres ORDER BY COALESCE((metadata->>'BlockCount')::int, 0)
		// DESC, updated_at DESC. BlockCount lives on FlowMetadata, which is
		// populated even when MetadataOnly is set, so this works for list views.
		slices.SortFunc(flows, func(a, b *interfaces.FlowDocument) int {
			if c := cmp.Compare(b.Metadata.BlockCount, a.Metadata.BlockCount); c != 0 {
				return c
			}
			return b.UpdatedAt.Compare(a.UpdatedAt)
		})
	default: // FlowSortUpdatedDesc / unset
		slices.SortFunc(flows, func(a, b *interfaces.FlowDocument) int {
			return b.UpdatedAt.Compare(a.UpdatedAt)
		})
	}
}

// CountFlows returns the total number of flows matching the filter, ignoring
// Limit/Offset. Uses a lightweight scan that only reads metadata fields rather
// than deserializing full flow content.
func (lsb *LocalStorageBackend) CountFlows(ctx context.Context, filter interfaces.FlowFilter) (int, error) {
	flowsDir := filepath.Join(lsb.dataDir, "flows")
	if _, err := os.Stat(flowsDir); os.IsNotExist(err) {
		return 0, nil
	}
	files, err := os.ReadDir(flowsDir)
	if err != nil {
		return 0, fmt.Errorf("failed to read flows directory: %w", err)
	}
	count := 0
	for _, file := range files {
		if file.IsDir() {
			continue
		}
		data, err := os.ReadFile(filepath.Join(flowsDir, file.Name())) // #nosec G304 -- enumerating the backend's own flows dir
		if err != nil {
			continue
		}
		var partial struct {
			OwnerID string `json:"owner_id"`
			OrgID   string `json:"org_id"`
			Name    string `json:"name"`
		}
		if err := json.Unmarshal(data, &partial); err != nil {
			continue
		}
		doc := &interfaces.FlowDocument{OwnerID: partial.OwnerID, OrganizationID: partial.OrgID, Name: partial.Name}
		if lsb.matchesFilter(doc, filter) {
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

// Ping checks if the storage backend is accessible
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
func (b *LocalStorageBackend) ListKnowledgeDocuments(ctx context.Context, orgID string) ([]*interfaces.KnowledgeDocument, error) {
	return nil, nil
}
func (b *LocalStorageBackend) SaveKnowledgeChunks(ctx context.Context, userID string, chunks []interfaces.KnowledgeChunk) error {
	return nil
}
func (b *LocalStorageBackend) SearchKnowledge(ctx context.Context, orgID string, queryEmbedding []float32, limit int) ([]interfaces.KnowledgeChunk, error) {
	return nil, nil
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

func (lsb *LocalStorageBackend) SaveUser(ctx context.Context, user *interfaces.User) error {
	user.Email = strings.ToLower(user.Email)
	lsb.mu.Lock()
	defer lsb.mu.Unlock()
	// Reject an email collision with a different user ID to mirror the Postgres
	// UNIQUE(email) constraint.
	for id, existing := range lsb.users {
		if existing.Email == user.Email && id != user.ID {
			return interfaces.ErrEmailExists
		}
	}
	// Key strictly by ID so each user is stored exactly once.
	lsb.users[user.ID] = user
	return nil
}

// CreateUser inserts a new user under the users mutex so the empty-check and
// the insert are atomic — two concurrent first-time registrations cannot both
// be promoted to RoleAdmin. Returns ErrEmailExists on email collision.
func (lsb *LocalStorageBackend) CreateUser(ctx context.Context, user *interfaces.User) error {
	user.Email = strings.ToLower(user.Email)
	lsb.mu.Lock()
	defer lsb.mu.Unlock()

	for _, existing := range lsb.users {
		if existing.Email == user.Email {
			return interfaces.ErrEmailExists
		}
	}

	role := user.Role
	if len(lsb.users) == 0 && auth.AllowBootstrap(ctx) {
		role = auth.RoleAdmin
	}
	user.Role = role
	lsb.users[user.ID] = user
	return nil
}

func (lsb *LocalStorageBackend) LoadUserByEmail(ctx context.Context, email string) (*interfaces.User, error) {
	email = strings.ToLower(email)
	lsb.mu.Lock()
	defer lsb.mu.Unlock()
	for _, u := range lsb.users {
		if u.Email == email {
			return u, nil
		}
	}
	return nil, interfaces.ErrNotFound
}

func (lsb *LocalStorageBackend) LoadUserByID(ctx context.Context, id string) (*interfaces.User, error) {
	lsb.mu.Lock()
	defer lsb.mu.Unlock()
	if u, ok := lsb.users[id]; ok {
		return u, nil
	}
	return nil, interfaces.ErrNotFound
}

// LoadUsersByIDs resolves multiple users via the in-memory id map (O(len(ids))).
func (lsb *LocalStorageBackend) LoadUsersByIDs(ctx context.Context, ids []string) (map[string]*interfaces.User, error) {
	lsb.mu.Lock()
	defer lsb.mu.Unlock()
	out := make(map[string]*interfaces.User, len(ids))
	for _, id := range ids {
		if u, ok := lsb.users[id]; ok {
			out[id] = u
		}
	}
	return out, nil
}

func (lsb *LocalStorageBackend) CountUsers(ctx context.Context) (int, error) {
	lsb.mu.Lock()
	defer lsb.mu.Unlock()
	return len(lsb.users), nil
}

func (lsb *LocalStorageBackend) ListUsers(ctx context.Context, limit, offset int) ([]*interfaces.User, error) {
	lsb.mu.Lock()
	defer lsb.mu.Unlock()
	users := make([]*interfaces.User, 0, len(lsb.users))
	for _, u := range lsb.users {
		users = append(users, u)
	}
	// Keep a stable, newest-first ordering to match the postgres backend.
	sort.Slice(users, func(i, j int) bool {
		return users[i].CreatedAt.After(users[j].CreatedAt)
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
	lsb.mu.Lock()
	defer lsb.mu.Unlock()
	var admins []*interfaces.User
	for _, u := range lsb.users {
		if u.Role == auth.RoleAdmin {
			admins = append(admins, u)
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

func (lsb *LocalStorageBackend) SaveOrg(ctx context.Context, org *interfaces.Organisation) error {
	lsb.mu.Lock()
	defer lsb.mu.Unlock()
	lsb.orgs[org.ID] = org
	return nil
}

func (lsb *LocalStorageBackend) LoadOrg(ctx context.Context, id string) (*interfaces.Organisation, error) {
	lsb.mu.RLock()
	defer lsb.mu.RUnlock()
	if o, ok := lsb.orgs[id]; ok {
		return o, nil
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
				result = append(result, o)
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

func (lsb *LocalStorageBackend) MutateOrg(ctx context.Context, id string, fn func(*interfaces.Organisation) error) error {
	lsb.mu.Lock()
	defer lsb.mu.Unlock()
	org, ok := lsb.orgs[id]
	if !ok {
		return interfaces.ErrNotFound
	}
	return fn(org)
}

// ---- Sharing operations ----

func (lsb *LocalStorageBackend) ListCollaborators(ctx context.Context, flowID string) ([]*interfaces.Collaborator, error) {
	lsb.mu.RLock()
	defer lsb.mu.RUnlock()
	collabs := lsb.sharing[flowID]
	if collabs == nil {
		return []*interfaces.Collaborator{}, nil
	}
	return collabs, nil
}

func (lsb *LocalStorageBackend) ListCollaboratorsBatch(ctx context.Context, flowIDs []string) (map[string][]*interfaces.Collaborator, error) {
	lsb.mu.RLock()
	defer lsb.mu.RUnlock()
	result := make(map[string][]*interfaces.Collaborator, len(flowIDs))
	for _, id := range flowIDs {
		if collabs := lsb.sharing[id]; len(collabs) > 0 {
			result[id] = collabs
		}
	}
	return result, nil
}

func (lsb *LocalStorageBackend) AddCollaborator(ctx context.Context, flowID string, c *interfaces.Collaborator) error {
	lsb.mu.Lock()
	defer lsb.mu.Unlock()
	if c.GrantedAt.IsZero() {
		c.GrantedAt = time.Now().UTC()
	}
	list := lsb.sharing[flowID]
	for i, existing := range list {
		if existing.UserID == c.UserID {
			list[i] = c
			return nil
		}
	}
	lsb.sharing[flowID] = append(list, c)
	return nil
}

func (lsb *LocalStorageBackend) UpdateCollaborator(ctx context.Context, flowID, userID string, permission string) error {
	lsb.mu.Lock()
	defer lsb.mu.Unlock()
	list := lsb.sharing[flowID]
	for _, existing := range list {
		if existing.UserID == userID {
			existing.Permission = permission
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
