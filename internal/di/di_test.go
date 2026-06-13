package di

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage/filesystem"
	storageif "pad-analyzer/internal/storage/interfaces"
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

	store, err := ProvideSettingsStore()
	if err != nil {
		t.Fatalf("ProvideSettingsStore() error = %v", err)
	}
	if store == nil {
		t.Fatal("ProvideSettingsStore() returned nil")
	}
}

func TestProvideConfigDir(t *testing.T) {
	t.Parallel()

	dir, err := ProvideConfigDir()
	if err != nil {
		t.Fatalf("ProvideConfigDir() error = %v", err)
	}
	if dir == "" {
		t.Fatal("ProvideConfigDir() returned empty string")
	}
}

func TestProvideASTCache(t *testing.T) {
	t.Parallel()

	c, err := ProvideASTCache()
	if err != nil {
		t.Fatalf("ProvideASTCache() error = %v", err)
	}
	if c == nil {
		t.Fatal("ProvideASTCache() returned nil")
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

	fs, err := filesystem.NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("create backend: %v", err)
	}
	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore())
	authz := service.NewAuthzService(fs, orgSvc)
	checker := ProvideFlowAccessChecker(authz, fs)
	if checker == nil {
		t.Fatal("expected non-nil checker with backend")
	}

	ctx := context.Background()
	doc := &storageif.FlowDocument{ID: "f1", Name: "f", OwnerID: "alice"}
	if err := fs.SaveFlow(ctx, doc); err != nil {
		t.Fatalf("seed flow: %v", err)
	}
	org, err := orgSvc.Create(ctx, "acme", "alice")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	orgDoc := &storageif.FlowDocument{ID: "f2", Name: "f", OwnerID: "alice", OrganizationID: org.ID}
	if err := fs.SaveFlow(ctx, orgDoc); err != nil {
		t.Fatalf("seed org flow: %v", err)
	}
	if err := orgSvc.AddMember(ctx, org.ID, "bob", auth.RoleViewer); err != nil {
		t.Fatalf("add member: %v", err)
	}

	if err := checker.CheckAccess(ctx, "f1", "alice"); err != nil {
		t.Errorf("owner should join: %v", err)
	}
	if err := checker.CheckAccess(ctx, "f2", "bob"); err != nil {
		t.Errorf("org member should join: %v", err)
	}
	if err := checker.CheckAccess(ctx, "f1", "mallory"); !errors.Is(err, wshub.ErrAccessDenied) {
		t.Errorf("expected ErrAccessDenied for stranger, got %v", err)
	}
	if err := checker.CheckAccess(ctx, "missing", "alice"); !errors.Is(err, wshub.ErrFlowNotFound) {
		t.Errorf("expected ErrFlowNotFound for missing flow, got %v", err)
	}
}
