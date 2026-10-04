package testutil

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// FakeBackend is an in-memory implementation of storageinterfaces.StorageBackend
// for tests: flows and settings in maps, everything else a no-ops zero value.
// Tests can directly manipulate the exported fields (Flows, Settings, PingErr).
type FakeBackend struct {
	Flows    map[string]*interfaces.FlowDocument
	Settings *interfaces.AppSettings
	PingErr  error
	// Conversations stores chat history keyed by scope+"\x00"+flowID; nil-safe
	// (the methods below tolerate a zero-valued struct-literal FakeBackend).
	Conversations map[string][]interfaces.ChatMessage
	// FlowTags backs the in-memory UpdateFlowTags (tag filter tests).
	FlowTags map[string][]string
	// OrgChannels backs the in-memory org-channel store (R2-3 tests).
	OrgChannels map[string]*interfaces.OrgChannel
	// OrgCustomRules backs the in-memory per-org custom-rule store (R4 tests).
	OrgCustomRules map[string]*interfaces.OrgCustomRule
	// KnowledgeDocs / KnowledgeChunks back the in-memory knowledge methods
	// (upload/replace/re-index round-trips in rag service tests).
	KnowledgeDocs   map[string]*interfaces.KnowledgeDocument
	KnowledgeChunks map[string]interfaces.KnowledgeChunk
	// EmbeddingDim is the deployment's vector width: a chunk counts as
	// vector-indexed (CountKnowledgeChunks, the sample's ordering) only when
	// its embedding has this width, mirroring the Postgres embedding_vec
	// rule. 0 keeps the any-embedding rule.
	EmbeddingDim int
	// UpdateKnowledgeEmbeddingsFailAt, when >0, makes the Nth (1-based) call
	// of UpdateKnowledgeChunkEmbeddings fail without writing — one re-index
	// page's commit failing; every other call writes (each call is its own
	// committed page). UpdateKnowledgeEmbeddingsCalls counts the calls.
	UpdateKnowledgeEmbeddingsFailAt int
	UpdateKnowledgeEmbeddingsCalls  int
	// FindingStatuses (flowID -> findingKey -> status) and Baselines (flowID ->
	// baseline) back the triage methods; lazily initialized for a struct literal.
	FindingStatuses map[string]map[string]*interfaces.FindingStatus
	Baselines       map[string]*interfaces.FlowBaseline
	// BatchSetFindingStatusErr, when non-nil, makes BatchSetFindingStatus fail
	// immediately. BatchSetFindingStatusFailAt, when >0, makes it fail at the
	// 0-based item index (proves the atomicity contract — items 0..K-1 must
	// NOT persist on a mid-batch failure).
	BatchSetFindingStatusErr    error
	BatchSetFindingStatusFailAt int
	// APITokens is keyed by token ID. Lazily initialized.
	APITokens map[string]*interfaces.APIToken
	// Users backs DeleteUser/ExportUserData for account-lifecycle tests;
	// lazily initialized. LoadUserByID/CreateUser stubs are left untouched to
	// avoid changing other suites' behaviour.
	Users        map[string]*interfaces.User
	DeletedUsers []string
	// UserTokens is keyed by token hash (password reset / email verify). Lazily
	// initialized.
	UserTokens map[string]*interfaces.UserToken
	// FlowHealth lets tests seed persisted per-flow health (flowID -> snapshot).
	FlowHealth map[string]*interfaces.HealthSnapshot
	// DailyUsage / UsageErr let tests drive GetDailyUsage (e.g. to verify the
	// AI budget check fails closed on a store error).
	DailyUsage float64
	UsageErr   error
	// UsageMetrics collects every SaveUsageMetric row in arrival order (the
	// audited provider records asynchronously, hence the mutex); read via
	// SavedUsageMetrics.
	UsageMetrics []*interfaces.UsageMetric
	usageMu      sync.Mutex
	// ShareTokens is keyed by token hash; lazily initialized.
	ShareTokens map[string]*interfaces.ShareToken
	// GovernanceAlerts backs the governance-alert inbox methods (id -> alert).
	// Lazily initialized; the methods are in-memory mirrors of the real store.
	GovernanceAlerts map[string]*interfaces.GovernanceAlert
	// Orgs backs SaveOrg/LoadOrg (id -> org). Lazily initialized; LoadOrg of
	// an unseeded id is ErrNotFound, as before.
	Orgs map[string]*interfaces.Organisation

	// Read counters for what one governance sweep costs (scanner tests):
	// each is incremented on every call and never reset; atomic, since
	// services load flows concurrently. ListFlowsContentBytes sums the
	// Content bytes ListFlows returned — a MetadataOnly page ships none.
	// ListFlowsFilterLog returns every ListFlows filter in call order.
	ListFlowsCalls             atomic.Int64
	ListFlowsContentBytes      atomic.Int64
	LoadFlowCalls              atomic.Int64
	GetFlowBaselineCalls       atomic.Int64
	GetFlowBaselinesBatchCalls atomic.Int64
	LoadFlowHealthCalls        atomic.Int64
	LoadFlowHealthBatchCalls   atomic.Int64
	LoadOrgCalls               atomic.Int64
	ListCollaboratorsCalls     atomic.Int64
	listFlowsFilters           []interfaces.FlowFilter // guarded by readsMu
	readsMu                    sync.Mutex
}

// ListFlowsFilterLog returns a copy of every filter ListFlows was called
// with, in call order.
func (m *FakeBackend) ListFlowsFilterLog() []interfaces.FlowFilter {
	m.readsMu.Lock()
	defer m.readsMu.Unlock()
	return append([]interfaces.FlowFilter(nil), m.listFlowsFilters...)
}

func NewFakeBackend() *FakeBackend {
	return &FakeBackend{
		Flows:         make(map[string]*interfaces.FlowDocument),
		Conversations: make(map[string][]interfaces.ChatMessage),
	}
}

func convKey(flowID, scope string) string { return scope + "\x00" + flowID }

func (m *FakeBackend) Ping(_ context.Context) error { return m.PingErr }
func (m *FakeBackend) Close() error                 { return nil }

// UpdateFlowTags normalizes and stores the tags in FlowTags.
func (m *FakeBackend) UpdateFlowTags(_ context.Context, flowID string, tags []string) error {
	normalized, err := interfaces.NormalizeFlowTags(tags)
	if err != nil {
		return err
	}
	if m.FlowTags == nil {
		m.FlowTags = make(map[string][]string)
	}
	m.FlowTags[flowID] = normalized
	return nil
}

func (m *FakeBackend) SaveFlow(_ context.Context, f *interfaces.FlowDocument) error {
	// Lazily init: a zero-valued struct-literal FakeBackend is a documented
	// supported construction (see Conversations).
	if m.Flows == nil {
		m.Flows = make(map[string]*interfaces.FlowDocument)
	}
	if existing, ok := m.Flows[f.ID]; ok {
		if f.Version > 0 && existing.Version != f.Version {
			return interfaces.ErrVersionConflict
		}
		f.Version = existing.Version + 1
	} else {
		f.Version = 0
	}
	cp := *f
	m.Flows[f.ID] = &cp
	return nil
}

func (m *FakeBackend) LoadFlow(_ context.Context, id string) (*interfaces.FlowDocument, error) {
	m.LoadFlowCalls.Add(1)
	if f, ok := m.Flows[id]; ok {
		cp := *f
		return &cp, nil
	}
	return nil, interfaces.ErrNotFound
}

func (m *FakeBackend) LoadFlowHeader(_ context.Context, id string) (*interfaces.FlowDocument, error) {
	if f, ok := m.Flows[id]; ok {
		cp := *f
		cp.Content = nil
		cp.Source = "" // like both real backends: a header carries no document
		return &cp, nil
	}
	return nil, interfaces.ErrNotFound
}

// ListFlows pages every stored flow in id order (whatever SortBy says). It
// honours the keyset cursor (AfterID: only ids after it, like Postgres's
// `id > $n`), Offset, Limit and MetadataOnly (rows carry no Content or
// Source), so a walker's paging can be tested.
func (m *FakeBackend) ListFlows(_ context.Context, filter interfaces.FlowFilter) ([]*interfaces.FlowDocument, error) {
	m.ListFlowsCalls.Add(1)
	m.readsMu.Lock()
	m.listFlowsFilters = append(m.listFlowsFilters, filter)
	m.readsMu.Unlock()
	var result []*interfaces.FlowDocument
	for _, f := range m.Flows {
		if filter.AfterID != "" && f.ID <= filter.AfterID {
			continue
		}
		cp := *f
		if filter.MetadataOnly {
			cp.Content = nil
			cp.Source = ""
		}
		result = append(result, &cp)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })

	offset := filter.Offset
	if offset >= len(result) {
		return nil, nil
	}
	result = result[offset:]
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	if limit < len(result) {
		result = result[:limit]
	}
	for _, f := range result {
		m.ListFlowsContentBytes.Add(int64(len(f.Content)))
	}
	return result, nil
}

func (m *FakeBackend) CountFlows(_ context.Context, _ interfaces.FlowFilter) (int, error) {
	return len(m.Flows), nil
}

func (m *FakeBackend) DeleteFlow(_ context.Context, id string) error {
	delete(m.Flows, id)
	return nil
}

func (m *FakeBackend) SaveSettings(_ context.Context, s *interfaces.AppSettings) error {
	cp := *s
	m.Settings = &cp
	return nil
}

func (m *FakeBackend) LoadSettings(_ context.Context) (*interfaces.AppSettings, error) {
	if m.Settings == nil {
		return &interfaces.AppSettings{Version: 1}, nil
	}
	cp := *m.Settings
	return &cp, nil
}

func (m *FakeBackend) SaveUserSettings(_ context.Context, _ string, s *interfaces.AppSettings) error {
	return m.SaveSettings(context.TODO(), s)
}

func (m *FakeBackend) LoadUserSettings(_ context.Context, _ string) (*interfaces.AppSettings, error) {
	return m.LoadSettings(context.TODO())
}

func (m *FakeBackend) SaveOrgSettings(_ context.Context, _ string, s *interfaces.AppSettings) error {
	return m.SaveSettings(context.TODO(), s)
}

func (m *FakeBackend) LoadOrgSettings(_ context.Context, _ string) (*interfaces.AppSettings, error) {
	return m.LoadSettings(context.TODO())
}

func (m *FakeBackend) SaveConversation(_ context.Context, flowID, scope string, messages []interfaces.ChatMessage) error {
	if m.Conversations == nil {
		m.Conversations = make(map[string][]interfaces.ChatMessage)
	}
	m.Conversations[convKey(flowID, scope)] = messages
	return nil
}

func (m *FakeBackend) LoadConversation(_ context.Context, flowID, scope string) ([]interfaces.ChatMessage, error) {
	return m.Conversations[convKey(flowID, scope)], nil
}

func (m *FakeBackend) DeleteConversation(_ context.Context, flowID, scope string) error {
	delete(m.Conversations, convKey(flowID, scope))
	return nil
}

// ---- User operations ----
func (m *FakeBackend) SaveUser(_ context.Context, _ *interfaces.User) error { return nil }
func (m *FakeBackend) CreateUser(_ context.Context, _ *interfaces.User) error {
	return nil
}
func (m *FakeBackend) LoadUserByEmail(_ context.Context, _ string) (*interfaces.User, error) {
	return nil, interfaces.ErrNotFound
}
func (m *FakeBackend) LoadUserByID(_ context.Context, _ string) (*interfaces.User, error) {
	return nil, interfaces.ErrNotFound
}
func (m *FakeBackend) LoadUsersByIDs(_ context.Context, _ []string) (map[string]*interfaces.User, error) {
	return map[string]*interfaces.User{}, nil
}
func (m *FakeBackend) CountUsers(_ context.Context) (int, error) { return 0, nil }
func (m *FakeBackend) ListUsers(_ context.Context, _, _ int) ([]*interfaces.User, error) {
	return nil, nil
}
func (m *FakeBackend) ListAdmins(_ context.Context) ([]*interfaces.User, error) { return nil, nil }
func (m *FakeBackend) UpdateUserRole(_ context.Context, _ string, _ auth.Role) error {
	return nil
}

// QueryUsers and LoadUserStats read the seeded Users map (the same set
// DeleteUser/ExportUserData use) so admin-list tests can run on the fake.
func (m *FakeBackend) QueryUsers(_ context.Context, q interfaces.UserQuery) ([]*interfaces.User, int, error) {
	page, total := q.Apply(m.userSlice())
	return page, total, nil
}

func (m *FakeBackend) LoadUserStats(_ context.Context) (*interfaces.UserStats, error) {
	return interfaces.UserStatsOf(m.userSlice(), time.Now()), nil
}

// userSlice copies the seeded users so a caller cannot mutate the fake's state.
func (m *FakeBackend) userSlice() []*interfaces.User {
	users := make([]*interfaces.User, 0, len(m.Users))
	for _, u := range m.Users {
		cp := *u
		users = append(users, &cp)
	}
	return users
}

func (m *FakeBackend) DeleteUser(_ context.Context, id string) error {
	if m.Users == nil {
		m.Users = make(map[string]*interfaces.User)
	}
	if _, ok := m.Users[id]; !ok {
		return nil // idempotent, matching the postgres implementation
	}
	delete(m.Users, id)
	m.DeletedUsers = append(m.DeletedUsers, id)
	return nil
}

func (m *FakeBackend) ExportUserData(_ context.Context, id string) (*interfaces.UserDataExport, error) {
	if m.Users == nil {
		return nil, interfaces.ErrNotFound
	}
	u, ok := m.Users[id]
	if !ok {
		return nil, interfaces.ErrNotFound
	}
	cp := *u
	return &interfaces.UserDataExport{User: &cp, ExportedAt: time.Now().UTC()}, nil
}

func (m *FakeBackend) PurgeExpiredData(_ context.Context, _ int) (*interfaces.PurgeResult, error) {
	return &interfaces.PurgeResult{}, nil
}
func (m *FakeBackend) UpdateUserPassword(_ context.Context, _, _ string) error { return nil }
func (m *FakeBackend) UpdateUserProfile(_ context.Context, _, _, _ string) error {
	return nil
}

// ---- Organisation operations ----
func (m *FakeBackend) SaveOrg(_ context.Context, org *interfaces.Organisation) error {
	if m.Orgs == nil {
		m.Orgs = make(map[string]*interfaces.Organisation)
	}
	cp := *org
	cp.Members = append([]interfaces.OrgMember(nil), org.Members...)
	m.Orgs[org.ID] = &cp
	return nil
}
func (m *FakeBackend) LoadOrg(_ context.Context, id string) (*interfaces.Organisation, error) {
	m.LoadOrgCalls.Add(1)
	if org, ok := m.Orgs[id]; ok {
		cp := *org
		cp.Members = append([]interfaces.OrgMember(nil), org.Members...)
		return &cp, nil
	}
	return nil, interfaces.ErrNotFound
}
func (m *FakeBackend) ListOrgsForUser(_ context.Context, _ string) ([]*interfaces.Organisation, error) {
	return nil, nil
}
func (m *FakeBackend) DeleteOrg(_ context.Context, _ string) error { return nil }

// ---- Sharing operations ----
func (m *FakeBackend) ListCollaborators(_ context.Context, _ string, _ int) ([]*interfaces.Collaborator, error) {
	m.ListCollaboratorsCalls.Add(1)
	return nil, nil
}
func (m *FakeBackend) ListCollaboratorsBatch(_ context.Context, _ []string) (map[string][]*interfaces.Collaborator, error) {
	return map[string][]*interfaces.Collaborator{}, nil
}
func (m *FakeBackend) AddCollaborator(_ context.Context, _ string, _ *interfaces.Collaborator) error {
	return nil
}
func (m *FakeBackend) UpdateCollaborator(_ context.Context, _, _ string, _ string) error {
	return nil
}
func (m *FakeBackend) RemoveCollaborator(_ context.Context, _, _ string) error { return nil }

// ---- Usage tracking ----
func (m *FakeBackend) SaveUsageMetric(_ context.Context, metric *interfaces.UsageMetric) error {
	if metric == nil {
		return nil
	}
	cp := *metric
	m.usageMu.Lock()
	m.UsageMetrics = append(m.UsageMetrics, &cp)
	m.usageMu.Unlock()
	return nil
}

// SavedUsageMetrics returns a snapshot of the rows SaveUsageMetric received.
func (m *FakeBackend) SavedUsageMetrics() []*interfaces.UsageMetric {
	m.usageMu.Lock()
	defer m.usageMu.Unlock()
	return append([]*interfaces.UsageMetric(nil), m.UsageMetrics...)
}
func (m *FakeBackend) GetDailyUsage(_ context.Context, _, _ string) (float64, error) {
	return m.DailyUsage, m.UsageErr
}

// ---- Dashboard ----
func (m *FakeBackend) SaveFlowAnalysis(_ context.Context, _ *interfaces.FlowAnalysis) error {
	return nil
}
func (m *FakeBackend) LoadFlowHealth(_ context.Context, flowID string) (*interfaces.HealthSnapshot, error) {
	m.LoadFlowHealthCalls.Add(1)
	if h, ok := m.FlowHealth[flowID]; ok {
		return h, nil
	}
	return nil, nil
}
func (m *FakeBackend) LoadFlowHealthBatch(_ context.Context, flowIDs []string) (map[string]*interfaces.HealthSnapshot, error) {
	m.LoadFlowHealthBatchCalls.Add(1)
	out := make(map[string]*interfaces.HealthSnapshot, len(flowIDs))
	for _, id := range flowIDs {
		if h, ok := m.FlowHealth[id]; ok {
			out[id] = h
		}
	}
	return out, nil
}
func (m *FakeBackend) FlowDashboardData(_ context.Context, _ string, _ int) (*interfaces.DashboardData, error) {
	return &interfaces.DashboardData{ByCategory: map[string]int{}}, nil
}
func (m *FakeBackend) FlowDashboardAdvanced(_ context.Context, _ string, _ int) (*interfaces.DashboardAdvancedData, error) {
	return &interfaces.DashboardAdvancedData{Security: interfaces.DashboardSecurity{}}, nil
}
func (m *FakeBackend) LoadAnalysisSnapshots(_ context.Context, _ []string) ([]interfaces.AnalysisSnapshotRow, error) {
	return []interfaces.AnalysisSnapshotRow{}, nil
}
func (m *FakeBackend) LoadAnalysisHistory(_ context.Context, _ interfaces.AnalyticsWindow) (*interfaces.AnalysisHistoryRows, error) {
	return &interfaces.AnalysisHistoryRows{}, nil
}
func (m *FakeBackend) LoadTriageAnalytics(_ context.Context, _ interfaces.AnalyticsWindow) (*interfaces.TriageRows, error) {
	return &interfaces.TriageRows{}, nil
}
func (m *FakeBackend) LoadUsageAnalytics(_ context.Context, _ string, _ []string, _ string, _ interfaces.AnalyticsWindow) (*interfaces.UsageRows, error) {
	return &interfaces.UsageRows{}, nil
}
func (m *FakeBackend) LoadActivityAnalytics(_ context.Context, _ string, _ interfaces.AnalyticsWindow, _ int) (*interfaces.ActivityRows, error) {
	return &interfaces.ActivityRows{}, nil
}
func (m *FakeBackend) LoadSecurityAnalytics(_ context.Context, _, _ string, _ bool) (*interfaces.SecurityRows, error) {
	return &interfaces.SecurityRows{}, nil
}
func (m *FakeBackend) StoreRefreshToken(_ context.Context, _ string, _ string, _ time.Time) error {
	return nil
}
func (m *FakeBackend) IsRefreshTokenValid(_ context.Context, _ string) (bool, error) {
	return false, nil
}
func (m *FakeBackend) RevokeRefreshToken(_ context.Context, _ string) error {
	return nil
}
func (m *FakeBackend) VerifyAndRevokeRefreshToken(_ context.Context, _ string) (*interfaces.RefreshTokenInfo, error) {
	return nil, interfaces.ErrTokenAlreadyRevoked
}
func (m *FakeBackend) RevokeUserRefreshTokens(_ context.Context, _ string) error {
	return nil
}
func (m *FakeBackend) ListUserRefreshTokens(_ context.Context, _ string) ([]*interfaces.RefreshTokenInfo, error) {
	return nil, nil
}
func (m *FakeBackend) RevokeRefreshTokenForUser(_ context.Context, _ string, _ string) error {
	return nil
}

// ---- Knowledge Base ----
// KnowledgeDocs / KnowledgeChunks back the knowledge methods in memory;
// lazily initialized, so a struct-literal FakeBackend works without them.
func (m *FakeBackend) SaveKnowledgeDocument(_ context.Context, _ string, doc *interfaces.KnowledgeDocument) error {
	m.initKnowledge()
	m.KnowledgeDocs[doc.ID] = doc
	return nil
}
func (m *FakeBackend) DeleteKnowledgeDocument(_ context.Context, _ string, orgID, id string) error {
	m.initKnowledge()
	if d, ok := m.KnowledgeDocs[id]; ok && d.OrgID == orgID {
		delete(m.KnowledgeDocs, id)
		for cid, c := range m.KnowledgeChunks {
			if c.DocID == id {
				delete(m.KnowledgeChunks, cid)
			}
		}
	}
	return nil
}
func (m *FakeBackend) DeleteKnowledgeDocumentByName(_ context.Context, orgID, filename string) error {
	m.initKnowledge()
	for id, d := range m.KnowledgeDocs {
		if d.OrgID == orgID && d.Filename == filename {
			_ = m.DeleteKnowledgeDocument(context.TODO(), "", orgID, id)
		}
	}
	return nil
}
func (m *FakeBackend) ListKnowledgeDocuments(_ context.Context, orgID string, limit, offset int) ([]*interfaces.KnowledgeDocument, error) {
	m.initKnowledge()
	var out []*interfaces.KnowledgeDocument
	for _, d := range m.KnowledgeDocs {
		if d.OrgID == orgID {
			cp := *d
			for _, c := range m.KnowledgeChunks {
				if c.DocID == d.ID {
					cp.ChunkCount++
				}
			}
			out = append(out, &cp)
		}
	}
	if offset > 0 {
		if offset >= len(out) {
			return nil, nil
		}
		out = out[offset:]
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (m *FakeBackend) ListKnowledgeDocumentIDsByFilename(_ context.Context, orgID, filename string) ([]string, int, error) {
	m.initKnowledge()
	var ids []string
	total := 0
	for _, d := range m.KnowledgeDocs {
		if d.OrgID != orgID {
			continue
		}
		total++
		if d.Filename == filename {
			ids = append(ids, d.ID)
		}
	}
	sort.Strings(ids)
	return ids, total, nil
}
func (m *FakeBackend) SaveKnowledgeChunks(_ context.Context, _ string, chunks []interfaces.KnowledgeChunk) error {
	m.initKnowledge()
	for _, c := range chunks {
		m.KnowledgeChunks[c.ID] = c
	}
	return nil
}
func (m *FakeBackend) SearchKnowledge(_ context.Context, _ string, _ []float32, _ int) ([]interfaces.KnowledgeChunk, error) {
	return nil, nil
}

// ListKnowledgeChunkContents mirrors the Postgres keyset page: the org's
// chunks strictly after the (DocID, ID) cursor, in (DocID, ID) order, capped
// at limit (limit <= 0 = all).
func (m *FakeBackend) ListKnowledgeChunkContents(_ context.Context, orgID string, after interfaces.KnowledgeChunkCursor, limit int) ([]interfaces.KnowledgeChunk, error) {
	m.initKnowledge()
	var out []interfaces.KnowledgeChunk
	for _, c := range m.KnowledgeChunks {
		d, ok := m.KnowledgeDocs[c.DocID]
		if !ok || d.OrgID != orgID {
			continue
		}
		if c.DocID < after.DocID || (c.DocID == after.DocID && c.ID <= after.ID) {
			continue
		}
		out = append(out, interfaces.KnowledgeChunk{ID: c.ID, DocID: c.DocID, Content: c.Content})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DocID != out[j].DocID {
			return out[i].DocID < out[j].DocID
		}
		return out[i].ID < out[j].ID
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

// vectorIndexed mirrors the Postgres embedding_vec rule: an embedded chunk
// is in the vector index when EmbeddingDim is unset or matches its width.
func (m *FakeBackend) vectorIndexed(c interfaces.KnowledgeChunk) bool {
	return len(c.Embedding) > 0 && (m.EmbeddingDim == 0 || len(c.Embedding) == m.EmbeddingDim)
}

// SampleKnowledgeChunkEmbeddings mirrors the Postgres sample: ONE chunk per
// document (the vector-indexed one first, then the lowest ID), documents
// whose pick is indexed first, then DocID order, capped at n — embeddings
// copied so callers cannot alias the stored slice.
func (m *FakeBackend) SampleKnowledgeChunkEmbeddings(_ context.Context, orgID string, n int) ([]interfaces.KnowledgeChunk, error) {
	m.initKnowledge()
	if n <= 0 {
		return nil, nil
	}
	var all []interfaces.KnowledgeChunk
	for _, c := range m.KnowledgeChunks {
		if d, ok := m.KnowledgeDocs[c.DocID]; ok && d.OrgID == orgID && len(c.Embedding) > 0 {
			all = append(all, interfaces.KnowledgeChunk{
				ID: c.ID, DocID: c.DocID, Content: c.Content,
				Embedding: append([]float32(nil), c.Embedding...),
			})
		}
	}
	// DISTINCT ON (doc_id) ... ORDER BY doc_id, (embedding_vec IS NULL), id
	sort.Slice(all, func(i, j int) bool {
		if all[i].DocID != all[j].DocID {
			return all[i].DocID < all[j].DocID
		}
		if vi, vj := m.vectorIndexed(all[i]), m.vectorIndexed(all[j]); vi != vj {
			return vi
		}
		return all[i].ID < all[j].ID
	})
	var out []interfaces.KnowledgeChunk
	for _, c := range all {
		if len(out) > 0 && out[len(out)-1].DocID == c.DocID {
			continue
		}
		out = append(out, c)
	}
	// ORDER BY unindexed, doc_id LIMIT n
	sort.SliceStable(out, func(i, j int) bool {
		if vi, vj := m.vectorIndexed(out[i]), m.vectorIndexed(out[j]); vi != vj {
			return vi
		}
		return out[i].DocID < out[j].DocID
	})
	if len(out) > n {
		out = out[:n]
	}
	return out, nil
}

// SampleKnowledgeChunkEmbeddingsUpTo mirrors the Postgres query: the org's
// embedded chunks at or before the (DocID, ID) cursor, nearest first, capped
// at n, embeddings copied.
func (m *FakeBackend) SampleKnowledgeChunkEmbeddingsUpTo(_ context.Context, orgID string, upTo interfaces.KnowledgeChunkCursor, n int) ([]interfaces.KnowledgeChunk, error) {
	m.initKnowledge()
	if n <= 0 {
		return nil, nil
	}
	var out []interfaces.KnowledgeChunk
	for _, c := range m.KnowledgeChunks {
		d, ok := m.KnowledgeDocs[c.DocID]
		if !ok || d.OrgID != orgID || len(c.Embedding) == 0 {
			continue
		}
		if c.DocID > upTo.DocID || (c.DocID == upTo.DocID && c.ID > upTo.ID) {
			continue
		}
		out = append(out, interfaces.KnowledgeChunk{
			ID: c.ID, DocID: c.DocID, Content: c.Content,
			Embedding: append([]float32(nil), c.Embedding...),
		})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].DocID != out[j].DocID {
			return out[i].DocID > out[j].DocID
		}
		return out[i].ID > out[j].ID
	})
	if len(out) > n {
		out = out[:n]
	}
	return out, nil
}
func (m *FakeBackend) UpdateKnowledgeChunkEmbeddings(_ context.Context, _ string, chunks []interfaces.KnowledgeChunk) error {
	m.initKnowledge()
	m.UpdateKnowledgeEmbeddingsCalls++
	if m.UpdateKnowledgeEmbeddingsFailAt > 0 && m.UpdateKnowledgeEmbeddingsCalls == m.UpdateKnowledgeEmbeddingsFailAt {
		return fmt.Errorf("fake: update knowledge chunk embeddings failed (call %d)", m.UpdateKnowledgeEmbeddingsCalls)
	}
	for _, c := range chunks {
		if old, ok := m.KnowledgeChunks[c.ID]; ok {
			old.Embedding = c.Embedding
			m.KnowledgeChunks[c.ID] = old
		}
	}
	return nil
}
func (m *FakeBackend) CountKnowledgeChunks(_ context.Context, orgID string) (int, int, error) {
	m.initKnowledge()
	total, indexed := 0, 0
	for _, c := range m.KnowledgeChunks {
		if d, ok := m.KnowledgeDocs[c.DocID]; ok && d.OrgID == orgID {
			total++
			if m.vectorIndexed(c) {
				indexed++
			}
		}
	}
	return total, indexed, nil
}
func (m *FakeBackend) initKnowledge() {
	if m.KnowledgeDocs == nil {
		m.KnowledgeDocs = make(map[string]*interfaces.KnowledgeDocument)
	}
	if m.KnowledgeChunks == nil {
		m.KnowledgeChunks = make(map[string]interfaces.KnowledgeChunk)
	}
}

// ---- Finding triage & baselines ----
func (m *FakeBackend) SetFindingStatus(_ context.Context, st *interfaces.FindingStatus) error {
	if m.FindingStatuses == nil {
		m.FindingStatuses = make(map[string]map[string]*interfaces.FindingStatus)
	}
	if m.FindingStatuses[st.FlowID] == nil {
		m.FindingStatuses[st.FlowID] = make(map[string]*interfaces.FindingStatus)
	}
	if st.UpdatedAt.IsZero() {
		st.UpdatedAt = time.Now()
	}
	cp := *st
	m.FindingStatuses[st.FlowID][st.FindingKey] = &cp
	return nil
}

// BatchSetFindingStatus mirrors the single-item loop but stages updates in a
// local copy so an injected failure mid-batch leaves NO partial state behind
// (matching the Postgres atomicity contract — a non-atomic fake would hide
// real bugs).
func (m *FakeBackend) BatchSetFindingStatus(_ context.Context, flowID, userID string, items []*interfaces.FindingStatus) error {
	if m.FindingStatuses == nil {
		m.FindingStatuses = make(map[string]map[string]*interfaces.FindingStatus)
	}
	if m.FindingStatuses[flowID] == nil {
		m.FindingStatuses[flowID] = make(map[string]*interfaces.FindingStatus)
	}

	// Stage into a local map; only commit to the live map on full success.
	staged := make(map[string]*interfaces.FindingStatus, len(items))
	now := time.Now()
	for i, st := range items {
		if m.BatchSetFindingStatusFailAt > 0 && i == m.BatchSetFindingStatusFailAt {
			return fmt.Errorf("injected failure at item %d", i)
		}
		if st == nil || st.FindingKey == "" {
			return fmt.Errorf("batch item %d: missing findingKey", i)
		}
		st.FlowID = flowID
		if st.UpdatedAt.IsZero() {
			st.UpdatedAt = now
		}
		cp := *st
		staged[st.FindingKey] = &cp
	}
	if m.BatchSetFindingStatusErr != nil {
		return m.BatchSetFindingStatusErr
	}
	for k, v := range staged {
		m.FindingStatuses[flowID][k] = v
	}
	return nil
}

func (m *FakeBackend) ListFindingStatuses(_ context.Context, flowID string) ([]*interfaces.FindingStatus, error) {
	byKey := m.FindingStatuses[flowID]
	out := make([]*interfaces.FindingStatus, 0, len(byKey))
	for _, v := range byKey {
		out = append(out, v)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FindingKey < out[j].FindingKey })
	return out, nil
}

func (m *FakeBackend) DeleteFindingStatus(_ context.Context, flowID, findingKey string) error {
	if byKey := m.FindingStatuses[flowID]; byKey != nil {
		delete(byKey, findingKey)
	}
	return nil
}

func (m *FakeBackend) GetFlowBaseline(_ context.Context, flowID string) (*interfaces.FlowBaseline, error) {
	m.GetFlowBaselineCalls.Add(1)
	if b, ok := m.Baselines[flowID]; ok {
		return b, nil
	}
	return nil, nil
}

func (m *FakeBackend) GetFlowBaselinesBatch(_ context.Context, flowIDs []string) (map[string]*interfaces.FlowBaseline, error) {
	m.GetFlowBaselinesBatchCalls.Add(1)
	out := make(map[string]*interfaces.FlowBaseline, len(flowIDs))
	for _, id := range flowIDs {
		if b, ok := m.Baselines[id]; ok {
			out[id] = b
		}
	}
	return out, nil
}

func (m *FakeBackend) SetFlowBaseline(_ context.Context, b *interfaces.FlowBaseline) error {
	if m.Baselines == nil {
		m.Baselines = make(map[string]*interfaces.FlowBaseline)
	}
	if b.CreatedAt.IsZero() {
		b.CreatedAt = time.Now()
	}
	cp := *b
	m.Baselines[b.FlowID] = &cp
	return nil
}

func (m *FakeBackend) ClearFlowBaseline(_ context.Context, flowID string) error {
	delete(m.Baselines, flowID)
	return nil
}

// ---- API tokens ----
func (m *FakeBackend) CreateAPIToken(_ context.Context, t *interfaces.APIToken) error {
	if m.APITokens == nil {
		m.APITokens = make(map[string]*interfaces.APIToken)
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	cp := *t
	m.APITokens[t.ID] = &cp
	return nil
}

func (m *FakeBackend) GetAPITokenByHash(_ context.Context, tokenHash string) (*interfaces.APIToken, error) {
	for _, t := range m.APITokens {
		if t.TokenHash == tokenHash {
			cp := *t
			return &cp, nil
		}
	}
	return nil, interfaces.ErrNotFound
}

func (m *FakeBackend) ListAPITokens(_ context.Context, userID string) ([]*interfaces.APIToken, error) {
	out := make([]*interfaces.APIToken, 0)
	for _, t := range m.APITokens {
		if t.UserID == userID {
			cp := *t
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *FakeBackend) DeleteAPIToken(_ context.Context, userID, id string) error {
	if t, ok := m.APITokens[id]; ok && t.UserID == userID {
		delete(m.APITokens, id)
	}
	return nil
}

func (m *FakeBackend) CreateUserToken(_ context.Context, t *interfaces.UserToken) error {
	if m.UserTokens == nil {
		m.UserTokens = make(map[string]*interfaces.UserToken)
	}
	cp := *t
	m.UserTokens[t.TokenHash] = &cp
	return nil
}

func (m *FakeBackend) ConsumeUserToken(_ context.Context, purpose, tokenHash string) (string, error) {
	t, ok := m.UserTokens[tokenHash]
	if !ok || t.Purpose != purpose || time.Now().After(t.ExpiresAt) {
		return "", interfaces.ErrNotFound
	}
	delete(m.UserTokens, tokenHash)
	return t.UserID, nil
}

func (m *FakeBackend) SetUserEmailVerified(_ context.Context, _ string) error { return nil }

// InvalidateUserTokens mirrors the real backend: removes every unused token of
// the given purposes for userID — assertable directly on the fake's state.
func (m *FakeBackend) InvalidateUserTokens(_ context.Context, userID string, purposes ...string) error {
	if len(purposes) == 0 {
		return nil
	}
	wanted := make(map[string]struct{}, len(purposes))
	for _, p := range purposes {
		wanted[p] = struct{}{}
	}
	for hash, t := range m.UserTokens {
		if t.UserID != userID {
			continue
		}
		if _, ok := wanted[t.Purpose]; ok {
			delete(m.UserTokens, hash)
		}
	}
	return nil
}

// ---- Audit log ----
func (m *FakeBackend) SaveAuditEvent(_ context.Context, _ *interfaces.AuditEvent) error {
	return nil
}
func (m *FakeBackend) ListAuditEvents(_ context.Context, _ interfaces.AuditFilter) ([]*interfaces.AuditEvent, error) {
	return []*interfaces.AuditEvent{}, nil
}

// ---- Flow versioning ----
func (m *FakeBackend) SaveFlowVersion(_ context.Context, _ *interfaces.FlowVersion) error {
	return nil
}
func (m *FakeBackend) ListFlowVersions(_ context.Context, _ string, _ int) ([]*interfaces.FlowVersion, error) {
	return []*interfaces.FlowVersion{}, nil
}
func (m *FakeBackend) LoadFlowVersion(_ context.Context, _ string, _ int) (*interfaces.FlowVersion, error) {
	return nil, interfaces.ErrNotFound
}

// ---- Finding comments ----
func (m *FakeBackend) AddFindingComment(_ context.Context, c *interfaces.FindingComment) error {
	return nil
}
func (m *FakeBackend) ListFindingComments(_ context.Context, _, _ string) ([]*interfaces.FindingComment, error) {
	return []*interfaces.FindingComment{}, nil
}
func (m *FakeBackend) DeleteFindingComment(_ context.Context, _, _, _ string) error {
	return nil
}

// ---- Share tokens ----
func (m *FakeBackend) CreateShareToken(_ context.Context, t *interfaces.ShareToken) error {
	if m.ShareTokens == nil {
		m.ShareTokens = make(map[string]*interfaces.ShareToken)
	}
	if t.CreatedAt.IsZero() {
		t.CreatedAt = time.Now()
	}
	cp := *t
	m.ShareTokens[t.TokenHash] = &cp
	return nil
}
func (m *FakeBackend) GetShareTokenByHash(_ context.Context, tokenHash string) (*interfaces.ShareToken, error) {
	if m.ShareTokens == nil {
		return nil, interfaces.ErrNotFound
	}
	t, ok := m.ShareTokens[tokenHash]
	if !ok {
		return nil, interfaces.ErrNotFound
	}
	if t.ExpiresAt != nil && t.ExpiresAt.Before(time.Now()) {
		return nil, interfaces.ErrNotFound
	}
	cp := *t
	return &cp, nil
}
func (m *FakeBackend) ListShareTokens(_ context.Context, flowID string) ([]*interfaces.ShareToken, error) {
	var out []*interfaces.ShareToken
	for _, t := range m.ShareTokens {
		if t.FlowID == flowID {
			cp := *t
			out = append(out, &cp)
		}
	}
	return out, nil
}
func (m *FakeBackend) RevokeShareToken(_ context.Context, flowID, tokenID string) error {
	for hash, t := range m.ShareTokens {
		if t.ID == tokenID && t.FlowID == flowID {
			delete(m.ShareTokens, hash)
		}
	}
	return nil
}

// Policies — in-memory stub for tests.
func (m *FakeBackend) SavePolicy(_ context.Context, p *models.Policy) error { return nil }
func (m *FakeBackend) GetPolicy(_ context.Context, _, _ string) (*models.Policy, error) {
	return nil, interfaces.ErrNotFound
}
func (m *FakeBackend) ListPolicies(_ context.Context, _ string) ([]*models.Policy, error) {
	return []*models.Policy{}, nil
}
func (m *FakeBackend) DeletePolicy(_ context.Context, _, _ string) error { return nil }

// Governance alerts — in-memory mirror of the real store (id -> alert).
func (m *FakeBackend) govAlerts() map[string]*interfaces.GovernanceAlert {
	if m.GovernanceAlerts == nil {
		m.GovernanceAlerts = make(map[string]*interfaces.GovernanceAlert)
	}
	return m.GovernanceAlerts
}

func (m *FakeBackend) RecordGovernanceAlert(_ context.Context, a *interfaces.GovernanceAlert) error {
	if a == nil || a.ID == "" {
		return fmt.Errorf("governance alert requires an id")
	}
	if a.CreatedAt.IsZero() {
		a.CreatedAt = time.Now().UTC()
	}
	cp := *a
	m.govAlerts()[a.ID] = &cp
	return nil
}

func (m *FakeBackend) ListGovernanceAlerts(_ context.Context, filter interfaces.GovernanceAlertFilter) ([]*interfaces.GovernanceAlert, error) {
	out := make([]*interfaces.GovernanceAlert, 0, len(m.GovernanceAlerts))
	for _, a := range m.govAlerts() {
		if !filter.IncludeDismissed && a.DismissedAt != nil {
			continue
		}
		// Targeted alerts are personal: visible only to their target.
		if a.TargetUser != "" && a.TargetUser != filter.UserID {
			continue
		}
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	limit := filter.Limit
	if limit <= 0 {
		limit = 50
	}
	if filter.Offset >= len(out) {
		return out[:0], nil
	}
	out = out[filter.Offset:]
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (m *FakeBackend) UnreadGovernanceAlertCount(ctx context.Context) (int, error) {
	return m.UnreadGovernanceAlertCountFor(ctx, "")
}

func (m *FakeBackend) UnreadGovernanceAlertCountFor(_ context.Context, userID string) (int, error) {
	n := 0
	for _, a := range m.govAlerts() {
		if a.ReadAt == nil && a.DismissedAt == nil && (a.TargetUser == "" || a.TargetUser == userID) {
			n++
		}
	}
	return n, nil
}

func (m *FakeBackend) MarkGovernanceAlertRead(_ context.Context, _, alertID string) error {
	now := time.Now().UTC()
	if a, ok := m.govAlerts()[alertID]; ok && a.ReadAt == nil {
		a.ReadAt = &now
	}
	return nil
}

func (m *FakeBackend) MarkAllGovernanceAlertsRead(_ context.Context, _ string) error {
	now := time.Now().UTC()
	for _, a := range m.govAlerts() {
		if a.ReadAt == nil && a.DismissedAt == nil {
			a.ReadAt = &now
		}
	}
	return nil
}

func (m *FakeBackend) DismissGovernanceAlert(_ context.Context, _, alertID string) error {
	now := time.Now().UTC()
	if a, ok := m.govAlerts()[alertID]; ok && a.DismissedAt == nil {
		a.DismissedAt = &now
	}
	return nil
}

func (m *FakeBackend) ClearGovernanceAlerts(_ context.Context, _ string) error {
	for id, a := range m.govAlerts() {
		if a.DismissedAt != nil {
			delete(m.GovernanceAlerts, id)
		}
	}
	return nil
}

// SaveOrgCustomRule backs the per-org custom-rule store in memory (R4 tests).
// The (OrgID, RuleID) uniqueness and the org-ownership guard mirror the
// Postgres upsert exactly — a fake laxer than the real backend would report
// success on precisely the cross-tenant write the real one rejects (B8).
func (m *FakeBackend) SaveOrgCustomRule(_ context.Context, rule *interfaces.OrgCustomRule) error {
	if m.OrgCustomRules == nil {
		m.OrgCustomRules = make(map[string]*interfaces.OrgCustomRule)
	}
	// Composite key wins: re-saving the same (org, ruleID) replaces in place.
	for _, existing := range m.OrgCustomRules {
		if existing.OrgID == rule.OrgID && existing.RuleID == rule.RuleID {
			rule.ID = existing.ID
			break
		}
	}
	if existing, ok := m.OrgCustomRules[rule.ID]; ok && existing.OrgID != rule.OrgID {
		return interfaces.ErrNotFound
	}
	cp := *rule
	m.OrgCustomRules[rule.ID] = &cp
	return nil
}

func (m *FakeBackend) DeleteOrgCustomRule(_ context.Context, orgID, id string) error {
	if r, ok := m.OrgCustomRules[id]; ok && r.OrgID == orgID {
		delete(m.OrgCustomRules, id)
	}
	return nil
}

func (m *FakeBackend) ListOrgCustomRules(_ context.Context, orgID string, enabledOnly bool) ([]*interfaces.OrgCustomRule, error) {
	var out []*interfaces.OrgCustomRule
	for _, r := range m.OrgCustomRules {
		if r.OrgID == orgID && (!enabledOnly || r.Enabled) {
			cp := *r
			out = append(out, &cp)
		}
	}
	// Deterministic order — the analysis path compiles these into a rule set
	// whose digest feeds the cache key; map-iteration order would make it
	// unstable across calls.
	sort.Slice(out, func(i, j int) bool {
		if !out[i].CreatedAt.Equal(out[j].CreatedAt) {
			return out[i].CreatedAt.Before(out[j].CreatedAt)
		}
		return out[i].ID < out[j].ID
	})
	return out, nil
}

// SaveOrgChannel backs the org-channel store in memory (R2-3 tests). The
// org-ownership check mirrors the Postgres upsert's
// `WHERE org_channels.org_id = EXCLUDED.org_id`: an id existing under a
// DIFFERENT org is ErrNotFound, never a silent cross-tenant overwrite.
func (m *FakeBackend) SaveOrgChannel(_ context.Context, ch *interfaces.OrgChannel) error {
	if m.OrgChannels == nil {
		m.OrgChannels = make(map[string]*interfaces.OrgChannel)
	}
	if existing, ok := m.OrgChannels[ch.ID]; ok && existing.OrgID != ch.OrgID {
		return interfaces.ErrNotFound
	}
	cp := *ch
	m.OrgChannels[ch.ID] = &cp
	return nil
}

func (m *FakeBackend) DeleteOrgChannel(_ context.Context, orgID, id string) error {
	if c, ok := m.OrgChannels[id]; ok && c.OrgID == orgID {
		delete(m.OrgChannels, id)
	}
	return nil
}

func (m *FakeBackend) ListOrgChannels(_ context.Context, orgID string, enabledOnly bool) ([]*interfaces.OrgChannel, error) {
	var out []*interfaces.OrgChannel
	for _, c := range m.OrgChannels {
		if c.OrgID == orgID && (!enabledOnly || c.Enabled) {
			cp := *c
			out = append(out, &cp)
		}
	}
	return out, nil
}

// SearchFlowContents: in-memory substring match over stored flow Content —
// mirrors the Postgres pushdown's semantics for service-level tests.
func (m *FakeBackend) SearchFlowContents(_ context.Context, filter interfaces.FlowFilter, needle string, limit int) ([]*interfaces.FlowDocument, error) {
	if needle == "" {
		return []*interfaces.FlowDocument{}, nil
	}
	if limit <= 0 {
		limit = 200
	}
	var out []*interfaces.FlowDocument
	for _, f := range m.Flows {
		if filter.UserID != "" && f.OwnerID != filter.UserID {
			continue
		}
		if strings.Contains(strings.ToLower(string(f.Content)), strings.ToLower(needle)) {
			cp := *f
			cp.Content = nil
			out = append(out, &cp)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].UpdatedAt.After(out[j].UpdatedAt) })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
