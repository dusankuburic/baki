package testutil

import (
	"context"
	"fmt"
	"sort"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
	"pad-core/models"
)

// FakeBackend is an in-memory implementation of storageinterfaces.StorageBackend
// for tests. It stores flows and settings in maps; all other operations are
// no-ops returning zero values. Tests can directly manipulate the exported
// fields (Flows, Settings, PingErr) to seed or inspect state.
type FakeBackend struct {
	Flows    map[string]*interfaces.FlowDocument
	Settings *interfaces.AppSettings
	PingErr  error
	// Conversations stores chat history keyed by scope+"\x00"+flowID so cloud-mode
	// conversation persistence can be round-tripped in tests. nil-safe: the methods
	// below tolerate a zero-valued FakeBackend built with a struct literal.
	Conversations map[string][]interfaces.ChatMessage
	// FindingStatuses (flowID -> findingKey -> status) and Baselines (flowID ->
	// baseline) back the triage methods. Lazily initialized, so a struct-literal
	// FakeBackend works without NewFakeBackend.
	FindingStatuses map[string]map[string]*interfaces.FindingStatus
	Baselines       map[string]*interfaces.FlowBaseline
	// BatchSetFindingStatusErr, when non-nil, makes BatchSetFindingStatus fail
	// immediately. BatchSetFindingStatusFailAt, when >0, makes it fail at the
	// 0-based item index (used to prove the atomicity contract — items 0..K-1
	// must NOT persist on a mid-batch failure).
	BatchSetFindingStatusErr    error
	BatchSetFindingStatusFailAt int
	// APITokens is keyed by token ID. Lazily initialized.
	APITokens map[string]*interfaces.APIToken
	// Users backs DeleteUser/ExportUserData for account-lifecycle tests. Lazily
	// initialized. (The existing LoadUserByID/CreateUser stubs are intentionally
	// left untouched to avoid changing other suites' behaviour.)
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
	// ShareTokens is keyed by token hash; lazily initialized.
	ShareTokens map[string]*interfaces.ShareToken
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

func (m *FakeBackend) SaveFlow(_ context.Context, f *interfaces.FlowDocument) error {
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
		return &cp, nil
	}
	return nil, interfaces.ErrNotFound
}

func (m *FakeBackend) ListFlows(_ context.Context, filter interfaces.FlowFilter) ([]*interfaces.FlowDocument, error) {
	var result []*interfaces.FlowDocument
	for _, f := range m.Flows {
		cp := *f
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
	return result, nil
}

func (m *FakeBackend) CountFlows(_ context.Context, _ interfaces.FlowFilter) (int, error) {
	return len(m.Flows), nil
}

func (m *FakeBackend) TransferFlowOwner(_ context.Context, _, _, _ string) error { return nil }

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
func (m *FakeBackend) SaveOrg(_ context.Context, _ *interfaces.Organisation) error { return nil }
func (m *FakeBackend) LoadOrg(_ context.Context, _ string) (*interfaces.Organisation, error) {
	return nil, interfaces.ErrNotFound
}
func (m *FakeBackend) ListOrgsForUser(_ context.Context, _ string) ([]*interfaces.Organisation, error) {
	return nil, nil
}
func (m *FakeBackend) DeleteOrg(_ context.Context, _ string) error { return nil }

// ---- Sharing operations ----
func (m *FakeBackend) ListCollaborators(_ context.Context, _ string) ([]*interfaces.Collaborator, error) {
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
func (m *FakeBackend) SaveUsageMetric(_ context.Context, _ *interfaces.UsageMetric) error {
	return nil
}
func (m *FakeBackend) GetDailyUsage(_ context.Context, _, _ string) (float64, error) {
	return m.DailyUsage, m.UsageErr
}

// ---- Dashboard ----
func (m *FakeBackend) SaveFlowAnalysis(_ context.Context, _ *interfaces.FlowAnalysis) error {
	return nil
}
func (m *FakeBackend) LoadFlowHealth(_ context.Context, flowID string) (*interfaces.HealthSnapshot, error) {
	if h, ok := m.FlowHealth[flowID]; ok {
		return h, nil
	}
	return nil, nil
}
func (m *FakeBackend) LoadFlowHealthBatch(_ context.Context, flowIDs []string) (map[string]*interfaces.HealthSnapshot, error) {
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
func (m *FakeBackend) SaveKnowledgeDocument(_ context.Context, _ *interfaces.KnowledgeDocument) error {
	return nil
}
func (m *FakeBackend) DeleteKnowledgeDocument(_ context.Context, _, _ string) error {
	return nil
}
func (m *FakeBackend) ListKnowledgeDocuments(_ context.Context, _ string) ([]*interfaces.KnowledgeDocument, error) {
	return nil, nil
}
func (m *FakeBackend) SaveKnowledgeChunks(_ context.Context, _ string, _ []interfaces.KnowledgeChunk) error {
	return nil
}
func (m *FakeBackend) SearchKnowledge(_ context.Context, _ string, _ []float32, _ int) ([]interfaces.KnowledgeChunk, error) {
	return nil, nil
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
// (matching the Postgres impl's atomicity contract). Atomicity is the whole
// point of the batch method — a non-atomic fake would hide real bugs.
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
	// Commit: merge staged into the live map.
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
	if b, ok := m.Baselines[flowID]; ok {
		return b, nil
	}
	return nil, nil
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
// the given purposes for userID. Stored in memory on the fake, so callers can
// assert post-state directly.
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
