package service

import (
	"context"
	"testing"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

func TestParseFlowSort(t *testing.T) {
	cases := []struct {
		input string
		want  storageif.FlowSort
	}{
		{"updated_asc", storageif.FlowSortUpdatedAsc},
		{"updated_desc", storageif.FlowSortUpdatedDesc},
		{"name_asc", storageif.FlowSortNameAsc},
		{"name_desc", storageif.FlowSortNameDesc},
		{"blocks_desc", storageif.FlowSortBlocksDesc},
		{"", storageif.FlowSortUpdatedDesc},      // empty → default
		{"bogus", storageif.FlowSortUpdatedDesc}, // unknown → default
	}
	for _, tc := range cases {
		if got := ParseFlowSort(tc.input); got != tc.want {
			t.Errorf("ParseFlowSort(%q) = %v, want %v", tc.input, got, tc.want)
		}
	}
}

func TestLibraryService_LocalMode_ListReturnsEmpty(t *testing.T) {
	svc := NewLibraryService(nil, nil, nil) // local mode
	docs, err := svc.ListLibraryFlows(context.Background(), "user1", "", ScopeAll, "", "", 10, 0)
	if err != nil {
		t.Fatalf("ListLibraryFlows: %v", err)
	}
	if len(docs) != 0 {
		t.Errorf("expected 0 docs in local mode, got %d", len(docs))
	}
}

func TestLibraryService_LocalMode_CountReturnsZero(t *testing.T) {
	svc := NewLibraryService(nil, nil, nil)
	count, err := svc.CountLibraryFlows(context.Background(), "user1", "", ScopeAll, "")
	if err != nil {
		t.Fatalf("CountLibraryFlows: %v", err)
	}
	if count != 0 {
		t.Errorf("expected 0 in local mode, got %d", count)
	}
}

func TestLibraryService_LocalMode_FlowHealthReturnsNil(t *testing.T) {
	svc := NewLibraryService(nil, nil, nil)
	h, err := svc.FlowHealth(context.Background(), "flow1")
	if err != nil {
		t.Fatalf("FlowHealth: %v", err)
	}
	if h != nil {
		t.Errorf("expected nil in local mode, got %+v", h)
	}
}

func TestLibraryService_LocalMode_PermissionsAllTrue(t *testing.T) {
	svc := NewLibraryService(nil, nil, nil)
	doc := &storageif.FlowDocument{ID: "f1"}
	canEdit, canDelete, canShare := svc.FlowPermissions(context.Background(), doc, "user1")
	if !canEdit || !canDelete || !canShare {
		t.Errorf("expected all true in local mode, got edit=%v delete=%v share=%v", canEdit, canDelete, canShare)
	}
}

func TestLibraryService_LocalMode_BatchPermissionsAllTrue(t *testing.T) {
	svc := NewLibraryService(nil, nil, nil)
	docs := []*storageif.FlowDocument{{ID: "f1"}, {ID: "f2"}}
	perms := svc.BatchFlowPermissions(context.Background(), docs, "user1")
	if len(perms) != 2 {
		t.Fatalf("expected 2 entries, got %d", len(perms))
	}
	for id, p := range perms {
		if !p.CanEdit || !p.CanDelete || !p.CanShare {
			t.Errorf("flow %s: expected all true, got edit=%v delete=%v share=%v", id, p.CanEdit, p.CanDelete, p.CanShare)
		}
	}
}

func TestLibraryService_LocalMode_CanReadCanWrite(t *testing.T) {
	svc := NewLibraryService(nil, nil, nil)
	doc := &storageif.FlowDocument{ID: "f1"}
	if !svc.CanRead(context.Background(), doc, "user1") {
		t.Error("expected CanRead=true in local mode")
	}
	if err := svc.CanWrite(context.Background(), doc, "user1"); err != nil {
		t.Errorf("expected CanWrite=nil in local mode, got %v", err)
	}
}

func TestResolveOwnerName_NilStorage(t *testing.T) {
	svc := NewLibraryService(nil, nil, nil)
	if name := svc.ResolveOwnerName(context.Background(), "user1"); name != "" {
		t.Errorf("expected empty name for nil storage, got %q", name)
	}
}

func TestResolveOwnerName_EmptyID(t *testing.T) {
	backend := &testutil.FakeBackend{}
	svc := NewLibraryService(backend, nil, nil)
	if name := svc.ResolveOwnerName(context.Background(), ""); name != "" {
		t.Errorf("expected empty name for empty ID, got %q", name)
	}
}

func TestResolveOwnerNames_NilStorage(t *testing.T) {
	svc := NewLibraryService(nil, nil, nil)
	names := svc.ResolveOwnerNames(context.Background(), []string{"u1", "u2"})
	if len(names) != 0 {
		t.Errorf("expected empty map for nil storage, got %v", names)
	}
}

func TestResolveOwnerNames_EmptyInput(t *testing.T) {
	backend := &testutil.FakeBackend{}
	svc := NewLibraryService(backend, nil, nil)
	names := svc.ResolveOwnerNames(context.Background(), nil)
	if len(names) != 0 {
		t.Errorf("expected empty map for empty input, got %v", names)
	}
}
