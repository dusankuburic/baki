package service

import (
	"testing"
	"time"

	"pad-analyzer/internal/events"
	"pad-analyzer/internal/platform/eventbus"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// TestLibraryMutations_AnnounceFlowChanged: library create, update (a version restore is one) and delete
// announce the change with the ids the flow caches need, the stored version after a write, and 0 after a delete.
func TestLibraryMutations_AnnounceFlowChanged(t *testing.T) {
	backend := testutil.NewFakeBackend()
	authz := NewAuthzService(backend, nil)
	bus := eventbus.New(nil, time.Second)
	rec := eventbus.NewRecorder(bus)
	lib := NewLibraryService(backend, bus, authz, nil)
	ctx := actorCtx("u1")

	saved, err := lib.CreateLibraryFlow(ctx, "u1", "", storageif.FlowDocument{Name: "New flow"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	saved.Name = "Renamed"
	if err := lib.UpdateLibraryFlow(ctx, saved); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := lib.DeleteLibraryFlow(ctx, saved.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}

	id := saved.ID
	want := []events.FlowChanged{
		{FlowID: id, StableID: id, OwnerID: "u1", Kind: events.FlowCreated, Version: 0},
		{FlowID: id, StableID: id, OwnerID: "u1", Kind: events.FlowUpdated, Version: int64(saved.Version)},
		{FlowID: id, StableID: id, OwnerID: "u1", Kind: events.FlowDeleted},
	}
	got := eventbus.Recorded[events.FlowChanged](rec)
	if len(got) != len(want) {
		t.Fatalf("announced %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("event %d = %+v, want %+v", i, got[i], want[i])
		}
	}
	if saved.Version == 0 {
		t.Error("fixture: the update did not bump the stored version")
	}
}
