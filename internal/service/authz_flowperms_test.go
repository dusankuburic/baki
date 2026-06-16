package service

import (
	"context"
	"testing"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// FlowPermissions computes the per-flow edit/delete/share capability flags used
// to populate library-list rows. canDelete is reserved for owners and org admins
// — an admin-tier collaborator may share but never delete.
func TestAuthz_FlowPermissions(t *testing.T) {
	authz, backend, orgSvc := newAuthzFixture(t)
	orgID := seedAuthzOrg(t, orgSvc) // alice=admin, bob=member, carol=viewer

	seedAuthzFlow(t, backend, "personal", "alice", "")
	seedAuthzFlow(t, backend, "ownerless", "", "")
	seedAuthzFlow(t, backend, "bob-org", "bob", orgID)
	addAuthzCollab(t, backend, "personal", "dave", "editor")
	addAuthzCollab(t, backend, "personal", "frank", "admin")

	ctx := context.Background()
	tests := []struct {
		name                            string
		flowID, owner, org, user        string
		wantEdit, wantDelete, wantShare bool
	}{
		{"owner has everything", "personal", "alice", "", "alice", true, true, true},
		{"ownerless flow grants nothing", "ownerless", "", "", "alice", false, false, false},
		{"editor collaborator edits only", "personal", "alice", "", "dave", true, false, false},
		{"admin collaborator shares but cannot delete", "personal", "alice", "", "frank", true, false, true},
		{"org admin (non-owner) has everything", "bob-org", "bob", orgID, "alice", true, true, true},
		{"unrelated user gets nothing", "personal", "alice", "", "stranger", false, false, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			edit, del, share := authz.FlowPermissions(ctx, tc.flowID, tc.owner, tc.org, tc.user)
			if edit != tc.wantEdit || del != tc.wantDelete || share != tc.wantShare {
				t.Errorf("FlowPermissions = (edit=%v delete=%v share=%v), want (edit=%v delete=%v share=%v)",
					edit, del, share, tc.wantEdit, tc.wantDelete, tc.wantShare)
			}
		})
	}
}

// BatchFlowPermissions must produce the same flags as FlowPermissions while
// resolving many flows in one pass.
func TestAuthz_BatchFlowPermissions_MatchesSingle(t *testing.T) {
	authz, backend, orgSvc := newAuthzFixture(t)
	orgID := seedAuthzOrg(t, orgSvc)

	seedAuthzFlow(t, backend, "personal", "alice", "")
	seedAuthzFlow(t, backend, "ownerless", "", "")
	seedAuthzFlow(t, backend, "bob-org", "bob", orgID)
	addAuthzCollab(t, backend, "personal", "dave", "editor")

	docs := []*storageif.FlowDocument{
		{ID: "personal", OwnerID: "alice"},
		{ID: "ownerless", OwnerID: ""},
		{ID: "bob-org", OwnerID: "bob", OrganizationID: orgID},
	}

	// As "dave": editor on personal, unrelated elsewhere.
	got := authz.BatchFlowPermissions(context.Background(), docs, "dave")
	want := map[string]PermFlags{
		"personal":  {CanEdit: true},
		"ownerless": {},
		"bob-org":   {},
	}
	for id, w := range want {
		if got[id] != w {
			t.Errorf("flow %q: got %+v, want %+v", id, got[id], w)
		}
	}

	// As the org admin "alice": owner of personal, org-admin over bob-org.
	gotAdmin := authz.BatchFlowPermissions(context.Background(), docs, "alice")
	if gotAdmin["bob-org"] != (PermFlags{CanEdit: true, CanDelete: true, CanShare: true}) {
		t.Errorf("alice over bob-org: got %+v, want all true (org admin)", gotAdmin["bob-org"])
	}
	if gotAdmin["personal"] != (PermFlags{CanEdit: true, CanDelete: true, CanShare: true}) {
		t.Errorf("alice over personal: got %+v, want all true (owner)", gotAdmin["personal"])
	}
}
