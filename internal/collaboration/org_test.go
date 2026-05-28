package collaboration

import (
	"testing"

	"pad-analyzer/internal/auth"
)

func newSvc() *OrgService { return NewOrgService(NewMemOrgStore()) }

// ---- Create ----

func TestCreate_ReturnsOrgWithOwnerAsAdmin(t *testing.T) {
	svc := newSvc()
	org, err := svc.Create("Acme Corp", "owner-1")
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if org.Name != "Acme Corp" {
		t.Errorf("Name: got %q", org.Name)
	}
	if org.OwnerID != "owner-1" {
		t.Errorf("OwnerID: got %q", org.OwnerID)
	}
	if len(org.Members) != 1 || org.Members[0].Role != auth.RoleAdmin {
		t.Error("owner should be the sole admin member")
	}
}

func TestCreate_EmptyNameReturnsError(t *testing.T) {
	_, err := newSvc().Create("", "u1")
	if err == nil {
		t.Error("expected error for empty name")
	}
}

func TestCreate_EmptyOwnerReturnsError(t *testing.T) {
	_, err := newSvc().Create("Test", "")
	if err == nil {
		t.Error("expected error for empty owner")
	}
}

// ---- Get ----

func TestGet_ExistingOrg(t *testing.T) {
	svc := newSvc()
	org, _ := svc.Create("Test", "u1")

	got, err := svc.Get(org.ID)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if got.ID != org.ID {
		t.Errorf("ID mismatch")
	}
}

func TestGet_NotFoundReturnsError(t *testing.T) {
	_, err := newSvc().Get("nonexistent")
	if err != ErrOrgNotFound {
		t.Errorf("expected ErrOrgNotFound, got %v", err)
	}
}

// ---- ListForUser ----

func TestListForUser_ReturnsOnlyUserOrgs(t *testing.T) {
	svc := newSvc()
	svc.Create("Org A", "alice")
	svc.Create("Org B", "bob")
	orgC, _ := svc.Create("Org C", "alice")

	list := svc.ListForUser("alice")
	if len(list) != 2 {
		t.Errorf("expected 2 orgs for alice, got %d", len(list))
	}
	_ = orgC
}

// ---- AddMember ----

func TestAddMember_Success(t *testing.T) {
	svc := newSvc()
	org, _ := svc.Create("Test", "owner")

	if err := svc.AddMember(org.ID, "new-user", auth.RoleViewer); err != nil {
		t.Fatalf("AddMember: %v", err)
	}

	if !svc.IsMember(org.ID, "new-user") {
		t.Error("new-user should be a member")
	}
}

func TestAddMember_DuplicateReturnsError(t *testing.T) {
	svc := newSvc()
	org, _ := svc.Create("Test", "owner")
	svc.AddMember(org.ID, "u2", auth.RoleMember)

	err := svc.AddMember(org.ID, "u2", auth.RoleMember)
	if err != ErrAlreadyMember {
		t.Errorf("expected ErrAlreadyMember, got %v", err)
	}
}

func TestAddMember_InvalidRoleReturnsError(t *testing.T) {
	svc := newSvc()
	org, _ := svc.Create("Test", "owner")
	if err := svc.AddMember(org.ID, "u2", "superuser"); err == nil {
		t.Error("expected error for invalid role")
	}
}

// ---- RemoveMember ----

func TestRemoveMember_Success(t *testing.T) {
	svc := newSvc()
	org, _ := svc.Create("Test", "owner")
	svc.AddMember(org.ID, "u2", auth.RoleMember)

	if err := svc.RemoveMember(org.ID, "u2"); err != nil {
		t.Fatalf("RemoveMember: %v", err)
	}
	if svc.IsMember(org.ID, "u2") {
		t.Error("u2 should no longer be a member")
	}
}

func TestRemoveMember_LastAdminBlocked(t *testing.T) {
	svc := newSvc()
	org, _ := svc.Create("Test", "owner")

	err := svc.RemoveMember(org.ID, "owner")
	if err != ErrLastAdmin {
		t.Errorf("expected ErrLastAdmin, got %v", err)
	}
}

func TestRemoveMember_NotFoundReturnsError(t *testing.T) {
	svc := newSvc()
	org, _ := svc.Create("Test", "owner")

	err := svc.RemoveMember(org.ID, "ghost")
	if err != ErrMemberNotFound {
		t.Errorf("expected ErrMemberNotFound, got %v", err)
	}
}

// ---- SetRole ----

func TestSetRole_Success(t *testing.T) {
	svc := newSvc()
	org, _ := svc.Create("Test", "owner")
	svc.AddMember(org.ID, "u2", auth.RoleMember)

	if err := svc.SetRole(org.ID, "u2", auth.RoleAdmin); err != nil {
		t.Fatalf("SetRole: %v", err)
	}

	role, _ := svc.MemberRole(org.ID, "u2")
	if role != auth.RoleAdmin {
		t.Errorf("expected admin, got %q", role)
	}
}

func TestSetRole_DemoteLastAdminBlocked(t *testing.T) {
	svc := newSvc()
	org, _ := svc.Create("Test", "owner")

	err := svc.SetRole(org.ID, "owner", auth.RoleMember)
	if err != ErrLastAdmin {
		t.Errorf("expected ErrLastAdmin, got %v", err)
	}
}

func TestSetRole_TwoAdmins_DemoteOneIsAllowed(t *testing.T) {
	svc := newSvc()
	org, _ := svc.Create("Test", "owner")
	svc.AddMember(org.ID, "u2", auth.RoleAdmin)

	if err := svc.SetRole(org.ID, "u2", auth.RoleMember); err != nil {
		t.Fatalf("should be able to demote when a second admin exists: %v", err)
	}
}

// ---- Delete ----

func TestDelete_RemovesOrg(t *testing.T) {
	svc := newSvc()
	org, _ := svc.Create("Test", "owner")

	if err := svc.Delete(org.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Get(org.ID); err != ErrOrgNotFound {
		t.Error("expected org to be gone after Delete")
	}
}

func TestDelete_NotFoundReturnsError(t *testing.T) {
	if err := newSvc().Delete("ghost"); err != ErrOrgNotFound {
		t.Errorf("expected ErrOrgNotFound, got %v", err)
	}
}
