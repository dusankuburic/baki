package migration_test

import (
	"context"
	"sort"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/migration"
	"pad-analyzer/internal/storage/interfaces"
)

// --- in-memory stub StorageBackend for tests ---

type memBackend struct {
	flows    map[string]*interfaces.FlowDocument
	settings *interfaces.AppSettings
	pingErr  error
}

func newMemBackend() *memBackend {
	return &memBackend{flows: make(map[string]*interfaces.FlowDocument)}
}

func (m *memBackend) Ping(_ context.Context) error { return m.pingErr }
func (m *memBackend) Close() error                  { return nil }

func (m *memBackend) SaveFlow(_ context.Context, f *interfaces.FlowDocument) error {
	cp := *f
	m.flows[f.ID] = &cp
	return nil
}

func (m *memBackend) LoadFlow(_ context.Context, id string) (*interfaces.FlowDocument, error) {
	if f, ok := m.flows[id]; ok {
		cp := *f
		return &cp, nil
	}
	return nil, interfaces.ErrNotFound
}

func (m *memBackend) ListFlows(_ context.Context, filter interfaces.FlowFilter) ([]*interfaces.FlowDocument, error) {
	var result []*interfaces.FlowDocument
	for _, f := range m.flows {
		cp := *f
		result = append(result, &cp)
	}
	// Deterministic order required for stable pagination
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

func (m *memBackend) DeleteFlow(_ context.Context, id string) error {
	delete(m.flows, id)
	return nil
}

func (m *memBackend) SaveSettings(_ context.Context, s *interfaces.AppSettings) error {
	cp := *s
	m.settings = &cp
	return nil
}

func (m *memBackend) LoadSettings(_ context.Context) (*interfaces.AppSettings, error) {
	if m.settings == nil {
		return &interfaces.AppSettings{Version: 1}, nil
	}
	cp := *m.settings
	return &cp, nil
}

func (m *memBackend) SaveUserSettings(_ context.Context, _ string, s *interfaces.AppSettings) error {
	return m.SaveSettings(nil, s)
}

func (m *memBackend) LoadUserSettings(_ context.Context, _ string) (*interfaces.AppSettings, error) {
	return m.LoadSettings(nil)
}

func (m *memBackend) SaveOrgSettings(_ context.Context, _ string, s *interfaces.AppSettings) error {
	return m.SaveSettings(nil, s)
}

func (m *memBackend) LoadOrgSettings(_ context.Context, _ string) (*interfaces.AppSettings, error) {
	return m.LoadSettings(nil)
}

func (m *memBackend) SaveConversation(_ context.Context, flowID, scope string, msgs []interfaces.ChatMessage) error {
	return nil
}

func (m *memBackend) LoadConversation(_ context.Context, flowID, scope string) ([]interfaces.ChatMessage, error) {
	return nil, nil
}

// ---- User operations ----
func (m *memBackend) SaveUser(ctx context.Context, user *interfaces.User) error { return nil }
func (m *memBackend) CreateUser(ctx context.Context, user *interfaces.User) error { return nil }
func (m *memBackend) LoadUserByEmail(ctx context.Context, email string) (*interfaces.User, error) { return nil, interfaces.ErrNotFound }
func (m *memBackend) LoadUserByID(ctx context.Context, id string) (*interfaces.User, error) { return nil, interfaces.ErrNotFound }
func (m *memBackend) LoadUsersByIDs(ctx context.Context, ids []string) (map[string]*interfaces.User, error) { return map[string]*interfaces.User{}, nil }
func (m *memBackend) CountUsers(ctx context.Context) (int, error) { return 0, nil }
func (m *memBackend) ListUsers(ctx context.Context) ([]*interfaces.User, error) { return nil, nil }

func (m *memBackend) ListAdmins(ctx context.Context) ([]*interfaces.User, error) { return nil, nil }
func (m *memBackend) UpdateUserRole(ctx context.Context, id string, role auth.Role) error { return nil }
func (m *memBackend) UpdateUserPassword(ctx context.Context, id string, passwordHash string) error { return nil }

// ---- Organisation operations ----
func (m *memBackend) SaveOrg(ctx context.Context, org *interfaces.Organisation) error { return nil }
func (m *memBackend) LoadOrg(ctx context.Context, id string) (*interfaces.Organisation, error) { return nil, interfaces.ErrNotFound }
func (m *memBackend) ListOrgsForUser(ctx context.Context, userID string) ([]*interfaces.Organisation, error) { return nil, nil }
func (m *memBackend) DeleteOrg(ctx context.Context, id string) error { return nil }

// ---- Sharing operations ----
func (m *memBackend) ListCollaborators(ctx context.Context, flowID string) ([]*interfaces.Collaborator, error) { return nil, nil }
func (m *memBackend) AddCollaborator(ctx context.Context, flowID string, c *interfaces.Collaborator) error { return nil }
func (m *memBackend) UpdateCollaborator(ctx context.Context, flowID, userID string, permission string) error { return nil }
func (m *memBackend) RemoveCollaborator(ctx context.Context, flowID, userID string) error { return nil }

// ---- Usage tracking ----
func (m *memBackend) SaveUsageMetric(ctx context.Context, metric *interfaces.UsageMetric) error { return nil }
func (m *memBackend) GetDailyUsage(ctx context.Context, userID, orgID string) (float64, error) { return 0, nil }

// ---- Knowledge Base ----
func (m *memBackend) SaveKnowledgeDocument(ctx context.Context, doc *interfaces.KnowledgeDocument) error { return nil }
func (m *memBackend) DeleteKnowledgeDocument(ctx context.Context, orgID, id string) error { return nil }
func (m *memBackend) ListKnowledgeDocuments(ctx context.Context, orgID string) ([]*interfaces.KnowledgeDocument, error) { return nil, nil }
func (m *memBackend) SaveKnowledgeChunks(ctx context.Context, chunks []interfaces.KnowledgeChunk) error { return nil }
func (m *memBackend) SearchKnowledge(ctx context.Context, orgID string, queryEmbedding []float32, limit int) ([]interfaces.KnowledgeChunk, error) { return nil, nil }

// ---- Audit log ----
func (m *memBackend) SaveAuditEvent(ctx context.Context, event *interfaces.AuditEvent) error { return nil }
func (m *memBackend) ListAuditEvents(ctx context.Context, filter interfaces.AuditFilter) ([]*interfaces.AuditEvent, error) { return []*interfaces.AuditEvent{}, nil }

// ---- Flow versioning ----
func (m *memBackend) SaveFlowVersion(ctx context.Context, v *interfaces.FlowVersion) error { return nil }
func (m *memBackend) ListFlowVersions(ctx context.Context, flowID string, limit int) ([]*interfaces.FlowVersion, error) { return []*interfaces.FlowVersion{}, nil }
func (m *memBackend) LoadFlowVersion(ctx context.Context, flowID string, version int) (*interfaces.FlowVersion, error) { return nil, interfaces.ErrNotFound }

// --- tests ---

func TestMigrator_MigratesFlows(t *testing.T) {
	src := newMemBackend()
	dst := newMemBackend()

	for i := 0; i < 3; i++ {
		id := string(rune('a' + i))
		src.flows[id] = &interfaces.FlowDocument{
			ID:        id,
			Name:      "Flow " + id,
			Content:   []byte(`{"subflows":[]}`),
			CreatedAt: time.Now(),
			UpdatedAt: time.Now(),
		}
	}

	res, err := migration.New(src, dst).Migrate(context.Background())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.FlowsMigrated != 3 {
		t.Errorf("FlowsMigrated: want 3, got %d", res.FlowsMigrated)
	}
	if res.FlowsFailed != 0 {
		t.Errorf("FlowsFailed: want 0, got %d", res.FlowsFailed)
	}
	if !res.SettingsMoved {
		t.Error("SettingsMoved should be true")
	}
	if len(dst.flows) != 3 {
		t.Errorf("dst flows: want 3, got %d", len(dst.flows))
	}
}

func TestMigrator_SkipsAlreadyMigrated(t *testing.T) {
	src := newMemBackend()
	dst := newMemBackend()

	flow := &interfaces.FlowDocument{
		ID: "existing", Name: "Existing",
		Content: []byte("{}"),
	}
	src.flows["existing"] = flow
	dst.flows["existing"] = flow // already there

	res, err := migration.New(src, dst).Migrate(context.Background())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.FlowsMigrated != 0 {
		t.Errorf("expected 0 migrated (already present), got %d", res.FlowsMigrated)
	}
}

func TestMigrator_InvalidFlowRecordedAsFailure(t *testing.T) {
	src := newMemBackend()
	dst := newMemBackend()

	src.flows["bad"] = &interfaces.FlowDocument{
		ID:      "bad",
		Name:    "", // invalid: no name
		Content: []byte("not-json"),
	}

	res, err := migration.New(src, dst).Migrate(context.Background())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.FlowsFailed != 1 {
		t.Errorf("FlowsFailed: want 1, got %d", res.FlowsFailed)
	}
	if len(res.Errors) != 1 {
		t.Errorf("expected 1 error entry, got %d", len(res.Errors))
	}
}

// TestMigrator_PartialFailure_RerunCompletes locks in the documented
// re-run model: when a first attempt fails some flows because the
// destination wasn't ready for them, fixing the underlying issue and
// re-running the migrator must complete the partial state without
// re-doing the already-migrated rows.
func TestMigrator_PartialFailure_RerunCompletes(t *testing.T) {
	src := newMemBackend()
	dst := newMemBackend()

	// Seed three flows; mark one as "bad" by giving it a malformed name
	// the validator rejects.
	src.flows["ok-1"] = &interfaces.FlowDocument{
		ID: "ok-1", Name: "Good Flow 1", Content: []byte(`{}`),
	}
	src.flows["bad"] = &interfaces.FlowDocument{
		ID: "bad", Name: "", Content: []byte("not-json"),
	}
	src.flows["ok-2"] = &interfaces.FlowDocument{
		ID: "ok-2", Name: "Good Flow 2", Content: []byte(`{}`),
	}

	// First run: 2 succeed, 1 fails validation.
	res, err := migration.New(src, dst).Migrate(context.Background())
	if err != nil {
		t.Fatalf("first run: %v", err)
	}
	if res.FlowsMigrated != 2 {
		t.Errorf("first run: migrated %d, want 2", res.FlowsMigrated)
	}
	if res.FlowsFailed != 1 {
		t.Errorf("first run: failed %d, want 1", res.FlowsFailed)
	}

	// Operator fixes the bad flow at the source and re-runs.
	src.flows["bad"].Name = "Fixed Name"
	src.flows["bad"].Content = []byte(`{}`)

	res2, err := migration.New(src, dst).Migrate(context.Background())
	if err != nil {
		t.Fatalf("second run: %v", err)
	}
	// The previously-migrated flows must be skipped (idempotent), and the
	// previously-failed flow must now succeed.
	if res2.FlowsMigrated != 1 {
		t.Errorf("rerun: migrated %d, want 1 (only the fixed flow)", res2.FlowsMigrated)
	}
	if res2.FlowsSkipped != 2 {
		t.Errorf("rerun: skipped %d, want 2 (the already-migrated flows)", res2.FlowsSkipped)
	}
	if res2.FlowsFailed != 0 {
		t.Errorf("rerun: failed %d, want 0", res2.FlowsFailed)
	}
	if len(dst.flows) != 3 {
		t.Errorf("dst should have all 3 flows after rerun, got %d", len(dst.flows))
	}
}

func TestMigrator_DestinationUnreachable(t *testing.T) {
	src := newMemBackend()
	dst := newMemBackend()
	dst.pingErr = context.DeadlineExceeded

	_, err := migration.New(src, dst).Migrate(context.Background())
	if err == nil {
		t.Fatal("expected error when destination unreachable")
	}
}

func TestMigrator_BatchSize(t *testing.T) {
	src := newMemBackend()
	dst := newMemBackend()

	for i := 0; i < 10; i++ {
		id := string(rune('a' + i))
		src.flows[id] = &interfaces.FlowDocument{
			ID: id, Name: "Flow " + id, Content: []byte("{}"),
		}
	}

	// Small batch to exercise multiple pages
	res, err := migration.New(src, dst).WithBatchSize(3).Migrate(context.Background())
	if err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	if res.FlowsMigrated != 10 {
		t.Errorf("FlowsMigrated: want 10, got %d", res.FlowsMigrated)
	}
}
