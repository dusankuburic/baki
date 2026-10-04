package service

import (
	"context"
	"testing"

	"pad-analyzer/internal/testutil"
)

// TestLoadFlowFiles_CloudReuploadKeepsTheFlowID: with no AST cache in cloud mode, an
// identical re-upload by the same caller still gets the id minted the first time while
// that flow exists (SaveUploadedFlow updates it rather than creating another); a
// different caller or content gets its own id; and once the flow is deleted an identical
// re-upload mints a NEW id — re-creating the old one would let a browser's content ETag
// (id + version) validate against a different row.
func TestLoadFlowFiles_CloudReuploadKeepsTheFlowID(t *testing.T) {
	backend := testutil.NewFakeBackend()
	svc := NewFlowService(&testutil.CountingNotifier{}, nil, NewCloudDocumentProvider(backend), backend, nil, nil)
	ctx := context.Background()
	files := map[string]string{"Main.txt": uploadFlowText}

	first, err := svc.LoadFlowFiles(ctx, files, "Payroll", "alice")
	if err != nil {
		t.Fatalf("upload: %v", err)
	}
	if err := svc.SaveUploadedFlow(ctx, first, "alice"); err != nil {
		t.Fatalf("persist: %v", err)
	}
	again, err := svc.LoadFlowFiles(ctx, files, "Payroll", "alice")
	if err != nil {
		t.Fatalf("re-upload: %v", err)
	}
	if again.ID != first.ID {
		t.Errorf("identical re-upload minted a new id %q, want %q", again.ID, first.ID)
	}
	other, err := svc.LoadFlowFiles(ctx, files, "Payroll", "bob")
	if err != nil {
		t.Fatalf("other caller: %v", err)
	}
	if other.ID == first.ID {
		t.Error("another caller's upload reused alice's flow id")
	}
	changed, err := svc.LoadFlowFiles(ctx, map[string]string{"Main.txt": uploadFlowText + "\n"}, "Payroll", "alice")
	if err != nil {
		t.Fatalf("changed upload: %v", err)
	}
	if changed.ID == first.ID {
		t.Error("different content reused the flow id")
	}

	if err := backend.DeleteFlow(ctx, first.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	afterDelete, err := svc.LoadFlowFiles(ctx, files, "Payroll", "alice")
	if err != nil {
		t.Fatalf("re-upload after delete: %v", err)
	}
	if afterDelete.ID == first.ID {
		t.Error("an identical re-upload re-created the deleted flow under its old id")
	}
}
