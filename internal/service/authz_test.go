package service

import (
	"context"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/storage/filesystem"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// newAuthzFixture builds an AuthzService over a real filesystem backend and a
// mem-backed OrgService, with one org ("org1", admin "alice", member "bob",
// viewer "carol") and one flow per scenario seeded by the caller.
func newAuthzFixture(t *testing.T) (*AuthzService, storageif.StorageBackend, *collaboration.OrgService) {
	t.Helper()
	fs, err := filesystem.NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("create backend: %v", err)
	}
	orgSvc := collaboration.NewOrgService(collaboration.NewMemOrgStore())
	return NewAuthzService(fs, orgSvc), fs, orgSvc
}

func seedAuthzOrg(t *testing.T, orgSvc *collaboration.OrgService) string {
	t.Helper()
	ctx := context.Background()
	org, err := orgSvc.Create(ctx, "org1", "alice")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(ctx, org.ID, "bob", auth.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if err := orgSvc.AddMember(ctx, org.ID, "carol", auth.RoleViewer); err != nil {
		t.Fatalf("add viewer: %v", err)
	}
	return org.ID
}

func seedAuthzFlow(t *testing.T, backend storageif.StorageBackend, id, ownerID, orgID string) {
	t.Helper()
	doc := &storageif.FlowDocument{ID: id, Name: "f", OwnerID: ownerID, OrganizationID: orgID}
	if err := backend.SaveFlow(context.Background(), doc); err != nil {
		t.Fatalf("seed flow: %v", err)
	}
}

func addAuthzCollab(t *testing.T, backend storageif.StorageBackend, flowID, userID, perm string) {
	t.Helper()
	c := &storageif.Collaborator{UserID: userID, Email: userID + "@x.test", Permission: perm}
	if err := backend.AddCollaborator(context.Background(), flowID, c); err != nil {
		t.Fatalf("add collaborator: %v", err)
	}
}

func TestAuthz_CheckFlowAccess_Matrix(t *testing.T) {
	authz, backend, orgSvc := newAuthzFixture(t)
	orgID := seedAuthzOrg(t, orgSvc)

	seedAuthzFlow(t, backend, "org-flow", "alice", orgID)
	seedAuthzFlow(t, backend, "personal-flow", "alice", "")
	seedAuthzFlow(t, backend, "ownerless-flow", "", "")
	addAuthzCollab(t, backend, "personal-flow", "dave", "editor")
	addAuthzCollab(t, backend, "personal-flow", "erin", "viewer")

	ctx := context.Background()
	cases := []struct {
		name    string
		flowID  string
		owner   string
		org     string
		user    string
		minPerm string
		allowed bool
	}{
		// Ownership
		{"owner has admin", "org-flow", "alice", orgID, "alice", "admin", true},
		// Org roles on org flows
		{"org member can read", "org-flow", "alice", orgID, "bob", "viewer", true},
		{"org member can edit", "org-flow", "alice", orgID, "bob", "editor", true},
		{"org member is not admin", "org-flow", "alice", orgID, "bob", "admin", false},
		{"org viewer can read", "org-flow", "alice", orgID, "carol", "viewer", true},
		{"org viewer cannot edit", "org-flow", "alice", orgID, "carol", "editor", false},
		{"non-member denied read", "org-flow", "alice", orgID, "mallory", "viewer", false},
		// Collaborator tiers on personal flows
		{"editor collab can read", "personal-flow", "alice", "", "dave", "viewer", true},
		{"editor collab can edit", "personal-flow", "alice", "", "dave", "editor", true},
		{"editor collab is not admin", "personal-flow", "alice", "", "dave", "admin", false},
		{"viewer collab can read", "personal-flow", "alice", "", "erin", "viewer", true},
		{"viewer collab cannot edit", "personal-flow", "alice", "", "erin", "editor", false},
		{"stranger denied", "personal-flow", "alice", "", "mallory", "viewer", false},
		// Ownerless legacy flows: read-only for everyone
		{"ownerless readable", "ownerless-flow", "", "", "anyone", "viewer", true},
		{"ownerless not writable", "ownerless-flow", "", "", "anyone", "editor", false},
		{"ownerless not admin", "ownerless-flow", "", "", "anyone", "admin", false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := authz.CheckFlowAccess(ctx, tc.flowID, tc.owner, tc.org, tc.user, tc.minPerm)
			if tc.allowed && err != nil {
				t.Errorf("expected allow, got %v", err)
			}
			if !tc.allowed && err == nil {
				t.Error("expected deny, got allow")
			}
		})
	}
}

func TestAuthz_CheckFlowAccessByID(t *testing.T) {
	authz, backend, orgSvc := newAuthzFixture(t)
	orgID := seedAuthzOrg(t, orgSvc)
	seedAuthzFlow(t, backend, "f1", "alice", orgID)

	ctx := context.Background()
	if err := authz.CheckFlowAccessByID(ctx, "f1", "bob", "viewer"); err != nil {
		t.Errorf("org member should read by ID: %v", err)
	}
	if err := authz.CheckFlowAccessByID(ctx, "f1", "mallory", "viewer"); err == nil {
		t.Error("stranger should be denied by ID")
	}
	if err := authz.CheckFlowAccessByID(ctx, "missing", "bob", "viewer"); err != storageif.ErrNotFound {
		t.Errorf("expected ErrNotFound for missing flow, got %v", err)
	}
}

func TestAuthz_CanDeleteFlow(t *testing.T) {
	authz, backend, orgSvc := newAuthzFixture(t)
	orgID := seedAuthzOrg(t, orgSvc)
	seedAuthzFlow(t, backend, "org-flow", "bob", orgID)
	addAuthzCollab(t, backend, "org-flow", "dave", "admin")

	ctx := context.Background()
	if !authz.CanDeleteFlow(ctx, "bob", orgID, "bob") {
		t.Error("owner should delete")
	}
	if !authz.CanDeleteFlow(ctx, "bob", orgID, "alice") {
		t.Error("org admin should delete")
	}
	if authz.CanDeleteFlow(ctx, "bob", orgID, "carol") {
		t.Error("org viewer should not delete")
	}
	// Collaborator-admin manages sharing but cannot destroy.
	if authz.CanDeleteFlow(ctx, "bob", "", "dave") {
		t.Error("collab admin should not delete")
	}
	if authz.CanDeleteFlow(ctx, "", "", "anyone") {
		t.Error("nobody owns an ownerless flow; deletion denied")
	}
}

func TestAuthz_OrgRequirements(t *testing.T) {
	authz, _, orgSvc := newAuthzFixture(t)
	orgID := seedAuthzOrg(t, orgSvc)
	ctx := context.Background()

	if err := authz.RequireOrgMember(ctx, orgID, "carol"); err != nil {
		t.Errorf("viewer is a member: %v", err)
	}
	if err := authz.RequireOrgMember(ctx, orgID, "mallory"); err == nil {
		t.Error("stranger is not a member")
	}
	if err := authz.RequireOrgAdmin(ctx, orgID, "alice"); err != nil {
		t.Errorf("alice is admin: %v", err)
	}
	if err := authz.RequireOrgAdmin(ctx, orgID, "bob"); err == nil {
		t.Error("bob is not admin")
	}
	if err := authz.RequireOrgWriter(ctx, orgID, "bob"); err != nil {
		t.Errorf("member is a writer: %v", err)
	}
	if err := authz.RequireOrgWriter(ctx, orgID, "carol"); err == nil {
		t.Error("viewer is not a writer")
	}
	if err := authz.RequireOrgWriter(ctx, orgID, "mallory"); err == nil {
		t.Error("stranger is not a writer")
	}
}

func TestAuthz_LocalModeAllowsEverything(t *testing.T) {
	authz := NewAuthzService(nil, nil)
	ctx := context.Background()

	if err := authz.CheckFlowAccess(ctx, "f", "someone-else", "org", "me", "admin"); err != nil {
		t.Errorf("local mode should allow: %v", err)
	}
	if err := authz.CheckFlowAccessByID(ctx, "missing", "me", "viewer"); err != nil {
		t.Errorf("local mode should allow by ID: %v", err)
	}
	if !authz.CanDeleteFlow(ctx, "someone-else", "", "me") {
		t.Error("local mode should allow delete")
	}
	if err := authz.RequireOrgMember(ctx, "org", "me"); err != nil {
		t.Errorf("local mode should allow org member: %v", err)
	}
	if err := authz.RequireOrgAdmin(ctx, "org", "me"); err != nil {
		t.Errorf("local mode should allow org admin: %v", err)
	}
	if err := authz.RequireOrgWriter(ctx, "org", "me"); err != nil {
		t.Errorf("local mode should allow org writer: %v", err)
	}
}
