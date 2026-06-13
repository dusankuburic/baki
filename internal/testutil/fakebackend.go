package testutil

import (
	"context"
	"sort"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// FakeBackend is an in-memory implementation of storageinterfaces.StorageBackend
// for tests. It stores flows and settings in maps; all other operations are
// no-ops returning zero values. Tests can directly manipulate the exported
// fields (Flows, Settings, PingErr) to seed or inspect state.
type FakeBackend struct {
	Flows    map[string]*interfaces.FlowDocument
	Settings *interfaces.AppSettings
	PingErr  error
}

func NewFakeBackend() *FakeBackend {
	return &FakeBackend{Flows: make(map[string]*interfaces.FlowDocument)}
}

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
	return m.SaveSettings(nil, s)
}

func (m *FakeBackend) LoadUserSettings(_ context.Context, _ string) (*interfaces.AppSettings, error) {
	return m.LoadSettings(nil)
}

func (m *FakeBackend) SaveOrgSettings(_ context.Context, _ string, s *interfaces.AppSettings) error {
	return m.SaveSettings(nil, s)
}

func (m *FakeBackend) LoadOrgSettings(_ context.Context, _ string) (*interfaces.AppSettings, error) {
	return m.LoadSettings(nil)
}

func (m *FakeBackend) SaveConversation(_ context.Context, _, _ string, _ []interfaces.ChatMessage) error {
	return nil
}

func (m *FakeBackend) LoadConversation(_ context.Context, _, _ string) ([]interfaces.ChatMessage, error) {
	return nil, nil
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
func (m *FakeBackend) CountUsers(_ context.Context) (int, error)             { return 0, nil }
func (m *FakeBackend) ListUsers(_ context.Context, _, _ int) ([]*interfaces.User, error) {
	return nil, nil
}
func (m *FakeBackend) ListAdmins(_ context.Context) ([]*interfaces.User, error) { return nil, nil }
func (m *FakeBackend) UpdateUserRole(_ context.Context, _ string, _ auth.Role) error {
	return nil
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
	return 0, nil
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
func (m *FakeBackend) SaveKnowledgeChunks(_ context.Context, _ []interfaces.KnowledgeChunk) error {
	return nil
}
func (m *FakeBackend) SearchKnowledge(_ context.Context, _ string, _ []float32, _ int) ([]interfaces.KnowledgeChunk, error) {
	return nil, nil
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
