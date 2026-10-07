package service

import (
	"slices"
	"testing"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// TestLibraryMutations_NotifyOwnerDashboard: library create, update and delete change the owner's flow set, so
// they fire the same mutation hook as FlowService's upload/save paths (DI wires it to the dashboard cache).
func TestLibraryMutations_NotifyOwnerDashboard(t *testing.T) {
	backend := testutil.NewFakeBackend()
	authz := NewAuthzService(backend, nil)
	flows := NewFlowService(&testutil.CountingNotifier{}, nil, NewCloudDocumentProvider(backend), backend, authz, nil)
	var notified []string
	flows.SetCloudFlowMutationHook(func(ownerID string) { notified = append(notified, ownerID) })
	lib := NewLibraryService(backend, flows, authz, nil)
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

	if want := []string{"u1", "u1", "u1"}; !slices.Equal(notified, want) {
		t.Errorf("dashboard notifications = %v, want %v (create, update, delete)", notified, want)
	}
}
