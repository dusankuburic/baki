package di

import (
	"testing"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/service"
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

	got := ProvideFlowAccessChecker(nil)
	if got != nil {
		t.Fatalf("expected nil when backend is nil, got %T", got)
	}
}
