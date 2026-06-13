package collaboration

import (
	"context"
	"errors"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

func now() time.Time { return time.Now().UTC() }

func TestMemOrgStore_SaveAndLoadOrg(t *testing.T) {
	store := NewMemOrgStore()
	ctx := context.Background()

	org := &interfaces.Organisation{
		ID:      "org-1",
		Name:    "Test Org",
		OwnerID: "user-1",
		Members: []interfaces.OrgMember{
			{UserID: "user-1", Role: auth.RoleAdmin, JoinedAt: now()},
		},
		CreatedAt: now(),
		UpdatedAt: now(),
	}

	if err := store.SaveOrg(ctx, org); err != nil {
		t.Fatalf("SaveOrg: %v", err)
	}

	loaded, err := store.LoadOrg(ctx, "org-1")
	if err != nil {
		t.Fatalf("LoadOrg: %v", err)
	}
	if loaded.Name != "Test Org" {
		t.Errorf("expected name 'Test Org', got %q", loaded.Name)
	}
	if len(loaded.Members) != 1 {
		t.Errorf("expected 1 member, got %d", len(loaded.Members))
	}
}

func TestMemOrgStore_LoadOrgNotFound(t *testing.T) {
	store := NewMemOrgStore()
	_, err := store.LoadOrg(context.Background(), "nonexistent")
	if !errors.Is(err, ErrOrgNotFound) {
		t.Errorf("expected ErrOrgNotFound, got %v", err)
	}
}

func TestMemOrgStore_SaveOrgDeepCopiesMembers(t *testing.T) {
	store := NewMemOrgStore()
	ctx := context.Background()

	org := &interfaces.Organisation{
		ID:      "org-1",
		Name:    "Test",
		OwnerID: "user-1",
		Members: []interfaces.OrgMember{
			{UserID: "user-1", Role: auth.RoleAdmin, JoinedAt: now()},
		},
		CreatedAt: now(),
		UpdatedAt: now(),
	}
	store.SaveOrg(ctx, org)

	org.Members[0].Role = auth.RoleViewer

	loaded, _ := store.LoadOrg(ctx, "org-1")
	if loaded.Members[0].Role != auth.RoleAdmin {
		t.Error("store should deep-copy members on save")
	}
}

func TestMemOrgStore_ListOrgsForUser(t *testing.T) {
	store := NewMemOrgStore()
	ctx := context.Background()

	store.SaveOrg(ctx, &interfaces.Organisation{
		ID: "org-1", Name: "Org 1", OwnerID: "user-1",
		Members: []interfaces.OrgMember{{UserID: "user-1", Role: auth.RoleAdmin, JoinedAt: now()}},
		CreatedAt: now(), UpdatedAt: now(),
	})
	store.SaveOrg(ctx, &interfaces.Organisation{
		ID: "org-2", Name: "Org 2", OwnerID: "user-2",
		Members: []interfaces.OrgMember{{UserID: "user-2", Role: auth.RoleAdmin, JoinedAt: now()}},
		CreatedAt: now(), UpdatedAt: now(),
	})
	store.SaveOrg(ctx, &interfaces.Organisation{
		ID: "org-3", Name: "Org 3", OwnerID: "user-2",
		Members: []interfaces.OrgMember{
			{UserID: "user-2", Role: auth.RoleAdmin, JoinedAt: now()},
			{UserID: "user-1", Role: auth.RoleMember, JoinedAt: now()},
		},
		CreatedAt: now(), UpdatedAt: now(),
	})

	orgs, err := store.ListOrgsForUser(ctx, "user-1")
	if err != nil {
		t.Fatalf("ListOrgsForUser: %v", err)
	}
	if len(orgs) != 2 {
		t.Errorf("expected 2 orgs for user-1, got %d", len(orgs))
	}
}

func TestMemOrgStore_ListOrgsForUser_NoMemberships(t *testing.T) {
	store := NewMemOrgStore()
	orgs, err := store.ListOrgsForUser(context.Background(), "lonely-user")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(orgs) != 0 {
		t.Errorf("expected 0 orgs, got %d", len(orgs))
	}
}

func TestMemOrgStore_DeleteOrg(t *testing.T) {
	store := NewMemOrgStore()
	ctx := context.Background()

	store.SaveOrg(ctx, &interfaces.Organisation{ID: "org-1", Name: "Test", OwnerID: "u1", CreatedAt: now(), UpdatedAt: now()})

	if err := store.DeleteOrg(ctx, "org-1"); err != nil {
		t.Fatalf("DeleteOrg: %v", err)
	}

	if _, err := store.LoadOrg(ctx, "org-1"); !errors.Is(err, ErrOrgNotFound) {
		t.Errorf("expected ErrOrgNotFound after delete, got %v", err)
	}
}

func TestMemOrgStore_DeleteOrgNotFound(t *testing.T) {
	store := NewMemOrgStore()
	err := store.DeleteOrg(context.Background(), "nonexistent")
	if !errors.Is(err, ErrOrgNotFound) {
		t.Errorf("expected ErrOrgNotFound, got %v", err)
	}
}

func TestMemOrgStore_MutateOrg(t *testing.T) {
	store := NewMemOrgStore()
	ctx := context.Background()

	store.SaveOrg(ctx, &interfaces.Organisation{ID: "org-1", Name: "Old", OwnerID: "u1", CreatedAt: now(), UpdatedAt: now()})

	err := store.MutateOrg(ctx, "org-1", func(org *interfaces.Organisation) error {
		org.Name = "New"
		return nil
	})
	if err != nil {
		t.Fatalf("MutateOrg: %v", err)
	}

	loaded, _ := store.LoadOrg(ctx, "org-1")
	if loaded.Name != "New" {
		t.Errorf("expected 'New', got %q", loaded.Name)
	}
}

func TestMemOrgStore_MutateOrgPropagatesError(t *testing.T) {
	store := NewMemOrgStore()
	ctx := context.Background()

	store.SaveOrg(ctx, &interfaces.Organisation{ID: "org-1", Name: "Test", OwnerID: "u1", CreatedAt: now(), UpdatedAt: now()})

	customErr := errors.New("mutation rejected")
	err := store.MutateOrg(ctx, "org-1", func(org *interfaces.Organisation) error {
		return customErr
	})
	if !errors.Is(err, customErr) {
		t.Errorf("expected custom error, got %v", err)
	}
}

func TestMemOrgStore_MutateOrgNotFound(t *testing.T) {
	store := NewMemOrgStore()
	err := store.MutateOrg(context.Background(), "nonexistent", func(org *interfaces.Organisation) error { return nil })
	if !errors.Is(err, ErrOrgNotFound) {
		t.Errorf("expected ErrOrgNotFound, got %v", err)
	}
}

func TestMemOrgStore_InviteSaveAndList(t *testing.T) {
	store := NewMemOrgStore()
	ctx := context.Background()

	invite := &interfaces.OrgInvite{
		ID: "inv-1", OrgID: "org-1", Email: "test@example.com",
		Role: auth.RoleMember, InvitedBy: "user-1",
		TokenHash: "hash-1", ExpiresAt: now().Add(24 * time.Hour),
		CreatedAt: now(),
	}

	if err := store.SaveOrgInvite(ctx, invite); err != nil {
		t.Fatalf("SaveOrgInvite: %v", err)
	}

	invites, err := store.ListOrgInvites(ctx, "org-1")
	if err != nil {
		t.Fatalf("ListOrgInvites: %v", err)
	}
	if len(invites) != 1 {
		t.Errorf("expected 1 invite, got %d", len(invites))
	}
}

func TestMemOrgStore_InviteDuplicateRejected(t *testing.T) {
	store := NewMemOrgStore()
	ctx := context.Background()

	invite := &interfaces.OrgInvite{
		ID: "inv-1", OrgID: "org-1", Email: "test@example.com",
		Role: auth.RoleMember, TokenHash: "hash-1",
		ExpiresAt: now().Add(24 * time.Hour), CreatedAt: now(),
	}
	store.SaveOrgInvite(ctx, invite)

	dup := &interfaces.OrgInvite{
		ID: "inv-2", OrgID: "org-1", Email: "test@example.com",
		Role: auth.RoleMember, TokenHash: "hash-2",
		ExpiresAt: now().Add(24 * time.Hour), CreatedAt: now(),
	}
	err := store.SaveOrgInvite(ctx, dup)
	if !errors.Is(err, interfaces.ErrOrgInviteExists) {
		t.Errorf("expected ErrOrgInviteExists, got %v", err)
	}
}

func TestMemOrgStore_GetOrgInviteByTokenHash(t *testing.T) {
	store := NewMemOrgStore()
	ctx := context.Background()

	invite := &interfaces.OrgInvite{
		ID: "inv-1", OrgID: "org-1", Email: "test@example.com",
		TokenHash: "secret-hash", ExpiresAt: now().Add(24 * time.Hour), CreatedAt: now(),
	}
	store.SaveOrgInvite(ctx, invite)

	found, err := store.GetOrgInviteByTokenHash(ctx, "secret-hash")
	if err != nil {
		t.Fatalf("GetOrgInviteByTokenHash: %v", err)
	}
	if found.ID != "inv-1" {
		t.Errorf("expected inv-1, got %s", found.ID)
	}
}

func TestMemOrgStore_GetOrgInviteByTokenHashNotFound(t *testing.T) {
	store := NewMemOrgStore()
	_, err := store.GetOrgInviteByTokenHash(context.Background(), "nonexistent")
	if !errors.Is(err, ErrInviteNotFound) {
		t.Errorf("expected ErrInviteNotFound, got %v", err)
	}
}

func TestMemOrgStore_DeleteInvite(t *testing.T) {
	store := NewMemOrgStore()
	ctx := context.Background()

	invite := &interfaces.OrgInvite{
		ID: "inv-1", OrgID: "org-1", Email: "test@example.com",
		TokenHash: "hash", ExpiresAt: now().Add(24 * time.Hour), CreatedAt: now(),
	}
	store.SaveOrgInvite(ctx, invite)

	if err := store.DeleteOrgInvite(ctx, "org-1", "inv-1"); err != nil {
		t.Fatalf("DeleteOrgInvite: %v", err)
	}

	invites, _ := store.ListOrgInvites(ctx, "org-1")
	if len(invites) != 0 {
		t.Errorf("expected 0 invites after delete, got %d", len(invites))
	}
}

func TestMemOrgStore_MarkOrgInviteAccepted(t *testing.T) {
	store := NewMemOrgStore()
	ctx := context.Background()

	invite := &interfaces.OrgInvite{
		ID: "inv-1", OrgID: "org-1", Email: "test@example.com",
		TokenHash: "hash", ExpiresAt: now().Add(24 * time.Hour), CreatedAt: now(),
	}
	store.SaveOrgInvite(ctx, invite)

	acceptTime := now()
	if err := store.MarkOrgInviteAccepted(ctx, "inv-1", acceptTime); err != nil {
		t.Fatalf("MarkOrgInviteAccepted: %v", err)
	}

	found, _ := store.GetOrgInvite(ctx, "org-1", "inv-1")
	if found.AcceptedAt == nil {
		t.Error("expected non-nil AcceptedAt")
	}
}

func TestMemOrgStore_AcceptedInviteAllowsDuplicateEmail(t *testing.T) {
	store := NewMemOrgStore()
	ctx := context.Background()

	invite := &interfaces.OrgInvite{
		ID: "inv-1", OrgID: "org-1", Email: "test@example.com",
		TokenHash: "hash-1", ExpiresAt: now().Add(24 * time.Hour), CreatedAt: now(),
	}
	store.SaveOrgInvite(ctx, invite)
	store.MarkOrgInviteAccepted(ctx, "inv-1", now())

	newInvite := &interfaces.OrgInvite{
		ID: "inv-2", OrgID: "org-1", Email: "test@example.com",
		TokenHash: "hash-2", ExpiresAt: now().Add(24 * time.Hour), CreatedAt: now(),
	}
	err := store.SaveOrgInvite(ctx, newInvite)
	if err != nil {
		t.Errorf("expected no error for duplicate email after acceptance, got %v", err)
	}
}
