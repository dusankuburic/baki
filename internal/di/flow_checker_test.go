package di

import (
	"context"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// CanEdit answers AuthzService.Can's FlowEdit for the socket's user: the owner and an org member edit, an org
// viewer and a stranger only read, and nobody edits a flow that does not exist.
func TestFlowAccessChecker_CanEditIsFlowEdit(t *testing.T) {
	t.Parallel()

	store := testutil.NewFakeBackend()
	ctx := context.Background()
	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore())
	org, err := orgSvc.Create(ctx, "acme", "alice")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	for user, role := range map[string]auth.Role{"bob": auth.RoleViewer, "carol": auth.RoleMember} {
		if err := orgSvc.AddMember(ctx, org.ID, user, role); err != nil {
			t.Fatalf("add %s: %v", user, err)
		}
	}
	if err := store.SaveFlow(ctx, &storageif.FlowDocument{ID: "f1", Name: "f", OwnerID: "alice", OrganizationID: org.ID}); err != nil {
		t.Fatalf("seed flow: %v", err)
	}
	checker := ProvideFlowAccessChecker(service.NewAuthzService(store, orgSvc), store)

	for user, want := range map[string]bool{"alice": true, "carol": true, "bob": false, "mallory": false} {
		if got := checker.CanEdit(ctx, "f1", user); got != want {
			t.Errorf("CanEdit(f1, %s) = %v, want %v", user, got, want)
		}
		if err := checker.CheckAccess(ctx, "f1", user); (err == nil) != (user != "mallory") {
			t.Errorf("CheckAccess(f1, %s) = %v", user, err)
		}
	}
	if checker.CanEdit(ctx, "missing", "alice") {
		t.Error("CanEdit on a missing flow = true")
	}
}
