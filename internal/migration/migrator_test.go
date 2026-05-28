package migration_test

import (
	"context"
	"sort"
	"testing"
	"time"

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

func (m *memBackend) SaveConversation(_ context.Context, flowID, scope string, msgs []interfaces.ChatMessage) error {
	return nil
}

func (m *memBackend) LoadConversation(_ context.Context, flowID, scope string) ([]interfaces.ChatMessage, error) {
	return nil, nil
}

// ---- User operations ----
func (m *memBackend) SaveUser(ctx context.Context, user *interfaces.User) error { return nil }
func (m *memBackend) LoadUserByEmail(ctx context.Context, email string) (*interfaces.User, error) { return nil, interfaces.ErrNotFound }
func (m *memBackend) LoadUserByID(ctx context.Context, id string) (*interfaces.User, error) { return nil, interfaces.ErrNotFound }
func (m *memBackend) CountUsers(ctx context.Context) (int, error) { return 0, nil }
func (m *memBackend) ListUsers(ctx context.Context) ([]*interfaces.User, error) { return nil, nil }

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
