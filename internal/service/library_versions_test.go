package service

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// versionFake numbers saved versions the way the stores do and keeps them for reading back.
type versionFake struct {
	*testutil.FakeBackend
	saved   []*storageif.FlowVersion
	saveErr error
}

func (f *versionFake) SaveFlowVersion(_ context.Context, v *storageif.FlowVersion) error {
	if f.saveErr != nil {
		return f.saveErr
	}
	v.Version = len(f.saved) + 1
	f.saved = append(f.saved, v)
	return nil
}

func (f *versionFake) ListFlowVersions(_ context.Context, flowID string, limit int) ([]*storageif.FlowVersion, error) {
	var out []*storageif.FlowVersion
	for _, v := range f.saved {
		if v.FlowID == flowID && (limit <= 0 || len(out) < limit) {
			out = append(out, v)
		}
	}
	return out, nil
}

func (f *versionFake) LoadFlowVersion(_ context.Context, flowID string, version int) (*storageif.FlowVersion, error) {
	for _, v := range f.saved {
		if v.FlowID == flowID && v.Version == version {
			return v, nil
		}
	}
	return nil, storageif.ErrNotFound
}

func TestLibraryService_FlowVersions(t *testing.T) {
	fake := &versionFake{FakeBackend: testutil.NewFakeBackend()}
	svc := NewLibraryService(fake, nil, nil, nil)
	ctx := context.Background()
	doc := &storageif.FlowDocument{
		ID:       "flow-1",
		Content:  json.RawMessage(`{"name":"f"}`),
		Metadata: storageif.FlowMetadata{BlockCount: 4},
	}

	v, err := svc.SaveFlowVersion(ctx, "flow-1", doc, "first", "u1")
	if err != nil {
		t.Fatalf("SaveFlowVersion: %v", err)
	}
	if v.ID == "" || v.FlowID != "flow-1" || v.Version != 1 || v.Comment != "first" || v.CreatedBy != "u1" ||
		v.CreatedAt.IsZero() || string(v.Content) != `{"name":"f"}` || v.Metadata.BlockCount != 4 {
		t.Fatalf("version = %+v", v)
	}
	if _, err := svc.SaveFlowVersion(ctx, "flow-1", doc, "second", "u1"); err != nil {
		t.Fatalf("second save: %v", err)
	}

	list, err := svc.ListFlowVersions(ctx, "flow-1", 1)
	if err != nil || len(list) != 1 {
		t.Fatalf("ListFlowVersions(limit 1) = %v, %v", list, err)
	}
	got, err := svc.LoadFlowVersion(ctx, "flow-1", 2)
	if err != nil || got.Comment != "second" {
		t.Fatalf("LoadFlowVersion(2) = %+v, %v", got, err)
	}
	if _, err := svc.LoadFlowVersion(ctx, "flow-1", 9); !errors.Is(err, storageif.ErrNotFound) {
		t.Fatalf("missing version: err = %v, want ErrNotFound", err)
	}

	fake.saveErr = errors.New("boom")
	if v, err := svc.SaveFlowVersion(ctx, "flow-1", doc, "", "u1"); err == nil || v != nil {
		t.Fatalf("failed save = %v, %v; want nil and the error", v, err)
	}
}

func TestFlowService_UpdateFlowTags(t *testing.T) {
	fake := testutil.NewFakeBackend()
	svc := NewFlowService(nil, nil, nil, fake, nil, nil)
	if err := fake.SaveFlow(context.Background(), &storageif.FlowDocument{ID: "flow-1", Name: "F", OwnerID: "u1"}); err != nil {
		t.Fatalf("seed flow: %v", err)
	}
	if err := svc.UpdateFlowTags(context.Background(), "flow-1", []string{"Finance", "finance", "ops"}); err != nil {
		t.Fatalf("UpdateFlowTags: %v", err)
	}
	want, _ := storageif.NormalizeFlowTags([]string{"Finance", "finance", "ops"})
	if got := fake.Flows["flow-1"].Tags; len(got) != len(want) {
		t.Fatalf("stored tags = %v, want %v", got, want)
	}
}
