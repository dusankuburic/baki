package filesystem

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// TestFlowVersionStorage covers the filesystem version store: versions are
// assigned sequentially (max+1), listed newest-first, and a missing version
// returns ErrNotFound. (These were previously no-op stubs; the restore/diff
// feature depends on real persistence.)
func TestFlowVersionStorage(t *testing.T) {
	fs, err := NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("NewLocalStorageBackend: %v", err)
	}
	ctx := context.Background()
	content := []byte(`{}`)

	v1 := &interfaces.FlowVersion{ID: "v1", FlowID: "f1", Comment: "first", Content: content}
	v2 := &interfaces.FlowVersion{ID: "v2", FlowID: "f1", Comment: "second", Content: content}
	if err := fs.SaveFlowVersion(ctx, v1); err != nil {
		t.Fatalf("SaveFlowVersion v1: %v", err)
	}
	if v1.Version != 1 {
		t.Errorf("first save Version = %d, want 1", v1.Version)
	}
	if err := fs.SaveFlowVersion(ctx, v2); err != nil {
		t.Fatalf("SaveFlowVersion v2: %v", err)
	}
	if v2.Version != 2 {
		t.Errorf("second save Version = %d, want 2", v2.Version)
	}

	// Load a specific version.
	got, err := fs.LoadFlowVersion(ctx, "f1", 1)
	if err != nil {
		t.Fatalf("LoadFlowVersion 1: %v", err)
	}
	if got.Comment != "first" {
		t.Errorf("LoadFlowVersion 1 Comment = %q, want \"first\"", got.Comment)
	}

	// Missing version → ErrNotFound.
	if _, err := fs.LoadFlowVersion(ctx, "f1", 99); !errors.Is(err, interfaces.ErrNotFound) {
		t.Errorf("LoadFlowVersion missing = %v, want ErrNotFound", err)
	}

	// List newest-first.
	list, err := fs.ListFlowVersions(ctx, "f1", 0)
	if err != nil {
		t.Fatalf("ListFlowVersions: %v", err)
	}
	if len(list) != 2 || list[0].Version != 2 {
		t.Errorf("ListFlowVersions = %d items, first Version %d; want 2 items, first Version 2", len(list), func() int {
			if len(list) > 0 {
				return list[0].Version
			}
			return 0
		}())
	}
}
