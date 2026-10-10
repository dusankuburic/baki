package di

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"pad-core/models"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	wshub "pad-analyzer/internal/websocket"
)

func TestProvideDocumentProvider(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mode       config.DeploymentMode
		wantNil    bool
		wantTypeOf interface{}
	}{
		{
			name:       "local mode returns LocalDocumentProvider",
			mode:       config.ModeLocal,
			wantNil:    false,
			wantTypeOf: &service.LocalDocumentProvider{},
		},
		{
			name:       "cloud mode with nil backend returns CloudDocumentProvider",
			mode:       config.ModeCloud,
			wantNil:    false,
			wantTypeOf: &service.CloudDocumentProvider{},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			got := ProvideDocumentProvider(tt.mode, nil)

			if tt.wantNil {
				if got != nil {
					t.Fatalf("expected nil, got %T", got)
				}
				return
			}
			if got == nil {
				t.Fatalf("expected non-nil result for mode %q", tt.mode)
			}
			switch tt.wantTypeOf.(type) {
			case *service.LocalDocumentProvider:
				if _, ok := got.(*service.LocalDocumentProvider); !ok {
					t.Fatalf("expected *service.LocalDocumentProvider, got %T", got)
				}
			case *service.CloudDocumentProvider:
				if _, ok := got.(*service.CloudDocumentProvider); !ok {
					t.Fatalf("expected *service.CloudDocumentProvider, got %T", got)
				}
			}
		})
	}
}

func TestProvideSettingsStore(t *testing.T) {
	t.Parallel()

	for _, mode := range []config.DeploymentMode{config.ModeLocal, config.ModeCloud} {
		store, err := ProvideSettingsStore(mode)
		if err != nil {
			t.Fatalf("ProvideSettingsStore(%v) error = %v", mode, err)
		}
		if store == nil {
			t.Fatalf("ProvideSettingsStore(%v) returned nil", mode)
		}
	}
}

// Cloud mode must not persist app settings to disk: deployment settings live
// in Postgres there, and the container has no writable home directory under the
// production compose overlay's read-only root filesystem. An Update that writes
// a file would fail with EROFS on boot.
func TestProvideSettingsStore_CloudDoesNotTouchDisk(t *testing.T) {
	// Not parallel: t.Setenv is incompatible with t.Parallel.

	// Point the config dir at a directory that does not exist and cannot be
	// created, so any filesystem access fails loudly instead of silently
	// landing somewhere harmless.
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(t.TempDir(), "nonexistent", "deeper"))

	store, err := ProvideSettingsStore(config.ModeCloud)
	if err != nil {
		t.Fatalf("ProvideSettingsStore(cloud) error = %v", err)
	}
	settings := store.Get()
	if settings == nil {
		t.Fatal("cloud settings store returned nil settings")
	}
	if err := store.Update(context.Background(), *settings); err != nil {
		t.Fatalf("cloud settings store Update wrote to disk: %v", err)
	}
}

func TestProvideConfigDir(t *testing.T) {
	t.Parallel()

	dir, err := ProvideConfigDir(config.ModeLocal)
	if err != nil {
		t.Fatalf("ProvideConfigDir(local) error = %v", err)
	}
	if dir == "" {
		t.Fatal("ProvideConfigDir(local) returned empty string")
	}

	// Cloud mode returns no path at all, so the stores that take it degrade to
	// no-ops rather than writing into an ephemeral container filesystem.
	cloudDir, err := ProvideConfigDir(config.ModeCloud)
	if err != nil {
		t.Fatalf("ProvideConfigDir(cloud) error = %v", err)
	}
	if cloudDir != "" {
		t.Fatalf("ProvideConfigDir(cloud) = %q, want empty", cloudDir)
	}
}

// An empty config dir must stay empty rather than becoming the relative path
// "analysis-history", which would write into the container's working directory.
func TestProvideHistoryStore_EmptyConfigDirIsNoOp(t *testing.T) {
	t.Parallel()

	store := ProvideHistoryStore("")
	if store == nil {
		t.Fatal("ProvideHistoryStore(\"\") returned nil")
	}
	// Record is a no-op for an empty dir; it must not panic or create files.
	store.Record("flow-1", &models.AnalysisReport{}, nil)

	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read working directory: %v", err)
	}
	for _, e := range entries {
		if e.Name() == "analysis-history" {
			t.Fatal("ProvideHistoryStore(\"\") created analysis-history in the working directory")
		}
	}
}

func TestProvideFlowAccessChecker_NilBackend(t *testing.T) {
	t.Parallel()

	got := ProvideFlowAccessChecker(nil, nil)
	if got != nil {
		t.Fatalf("expected nil when backend is nil, got %T", got)
	}
}

// TestFlowAccessChecker_TranslatesServiceErrors verifies the DI adapter maps
// service-layer authz errors onto the websocket package's sentinels.
func TestFlowAccessChecker_TranslatesServiceErrors(t *testing.T) {
	t.Parallel()

	store := testutil.NewFakeBackend()
	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore())
	authz := service.NewAuthzService(store, orgSvc)
	checker := ProvideFlowAccessChecker(authz, store)
	if checker == nil {
		t.Fatal("expected non-nil checker with backend")
	}

	ctx := context.Background()
	doc := &storageif.FlowDocument{ID: "f1", Name: "f", OwnerID: "alice"}
	if err := store.SaveFlow(ctx, doc); err != nil {
		t.Fatalf("seed flow: %v", err)
	}
	org, err := orgSvc.Create(ctx, "acme", "alice")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	orgDoc := &storageif.FlowDocument{ID: "f2", Name: "f", OwnerID: "alice", OrganizationID: org.ID}
	if err := store.SaveFlow(ctx, orgDoc); err != nil {
		t.Fatalf("seed org flow: %v", err)
	}
	if err := orgSvc.AddMember(ctx, org.ID, "bob", auth.RoleViewer); err != nil {
		t.Fatalf("add member: %v", err)
	}

	if _, err := checker.CheckAccess(ctx, "f1", sessionActor("alice")); err != nil {
		t.Errorf("owner should join: %v", err)
	}
	// The owner and organisation it read are what the hub's access-change rechecks match on.
	if owner, err := checker.CheckAccess(ctx, "f2", sessionActor("bob")); err != nil || owner != (wshub.FlowOwner{UserID: "alice", OrgID: org.ID}) {
		t.Errorf("org member should join, with the flow's owner and organisation: %+v, %v", owner, err)
	}
	if _, err := checker.CheckAccess(ctx, "f1", sessionActor("mallory")); !errors.Is(err, wshub.ErrAccessDenied) {
		t.Errorf("expected ErrAccessDenied for stranger, got %v", err)
	}
	if _, err := checker.CheckAccess(ctx, "missing", sessionActor("alice")); !errors.Is(err, wshub.ErrFlowNotFound) {
		t.Errorf("expected ErrFlowNotFound for missing flow, got %v", err)
	}
}

// TestProvideSettingsProvider_CloudUsesBackend pins the wiring that was wrong:
// in cloud mode, deployment-wide settings must come from the storage backend,
// not from the local file store. They used to come from the store, which in a
// container is recreated from defaults on every start — so an administrator
// could save a rule override, an AI daily budget or an embedding provider and
// the analysis path would keep reading the defaults.
func TestProvideSettingsProvider_CloudUsesBackend(t *testing.T) {
	t.Parallel()

	backend := &testutil.FakeBackend{}
	stored := models.DefaultSettings()
	stored.AI.DailyBudget = 100
	if err := backend.SaveSettings(context.Background(), toStorageSettings(t, stored)); err != nil {
		t.Fatalf("seed backend settings: %v", err)
	}

	store := storage.NewInMemorySettingsStore()
	provider := ProvideSettingsProvider(context.Background(), config.ModeCloud, store, backend)

	if got := provider.Get().AI.DailyBudget; got != 100 {
		t.Errorf("cloud settings provider returned DailyBudget %v, want the backend's 100 "+
			"(a default here means it is reading the local store again)", got)
	}
	if provider == service.SettingsProvider(store) {
		t.Error("cloud mode must not use the local settings store")
	}
}

// TestProvideSettingsProvider_LocalUsesStore is the other half: the desktop has
// no storage backend, and its settings belong in the user's config directory.
func TestProvideSettingsProvider_LocalUsesStore(t *testing.T) {
	t.Parallel()

	store := storage.NewInMemorySettingsStore()
	provider := ProvideSettingsProvider(context.Background(), config.ModeLocal, store, nil)
	if provider != service.SettingsProvider(store) {
		t.Error("local mode should use the file-backed settings store")
	}

	// And a backend being present does not change that: only cloud mode moves
	// deployment settings into the database.
	provider = ProvideSettingsProvider(context.Background(), config.ModeLocal, store, &testutil.FakeBackend{})
	if provider != service.SettingsProvider(store) {
		t.Error("local mode with a backend present should still use the settings store")
	}
}

// toStorageSettings converts via JSON, mirroring how the service layer bridges
// the domain and storage settings structs.
func toStorageSettings(t *testing.T, m *models.AppSettings) *storageif.AppSettings {
	t.Helper()
	data, err := json.Marshal(m)
	if err != nil {
		t.Fatalf("marshal settings: %v", err)
	}
	var is storageif.AppSettings
	if err := json.Unmarshal(data, &is); err != nil {
		t.Fatalf("unmarshal settings: %v", err)
	}
	return &is
}
