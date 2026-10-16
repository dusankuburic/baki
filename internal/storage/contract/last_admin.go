package contract

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// runLastAdminScenarios holds UpdateUserRole to the deployment keeping an admin: the last one keeps the role.
// (The concurrent race between two demotions is guarded by user_role_locks_test.go and account_races_test.go with
// explicit row-lock hold barriers; the contract checks the functional requirement on a fresh store).
func runLastAdminScenarios(ctx context.Context, fresh func(*testing.T) interfaces.StorageBackend, runID string, run func(name string, fn func(t *testing.T))) {
	run("UpdateUserRole_keeps_the_last_admin", func(t *testing.T) {
		if fresh == nil {
			t.Fatal("this scenario needs an empty store: pass contract.FreshStore to RunSuite")
		}
		b := fresh(t)
		x, y := "contract-la-x-"+runID, "contract-la-y-"+runID
		for _, id := range []string{x, y} {
			if err := b.CreateUser(ctx, &interfaces.User{ID: id, Email: id + "@example.com", Password: "hash", Role: auth.RoleAdmin}); err != nil {
				t.Fatalf("CreateUser %s: %v", id, err)
			}
		}
		if admins, err := b.ListAdmins(ctx); err != nil || len(admins) != 2 {
			t.Fatalf("the fresh store's admins = %d, %v; want only the scenario's two", len(admins), err)
		}

		// Demotion with another admin left goes through; the last admin's is refused; a role change to admin needs no check.
		if err := b.UpdateUserRole(ctx, x, auth.RoleMember); err != nil {
			t.Fatalf("demote %s while %s is an admin: %v", x, y, err)
		}
		if err := b.UpdateUserRole(ctx, y, auth.RoleMember); !errors.Is(err, interfaces.ErrLastAdmin) {
			t.Errorf("demote the last admin: %v, want ErrLastAdmin", err)
		}
		if u, err := b.LoadUserByID(ctx, y); err != nil || u.Role != auth.RoleAdmin {
			t.Errorf("the refused demotion changed the role: %+v, %v", u, err)
		}
		// A change between two other roles is no demotion.
		if err := b.UpdateUserRole(ctx, x, auth.RoleViewer); err != nil {
			t.Errorf("make the member %s a viewer: %v", x, err)
		}
		if err := b.UpdateUserRole(ctx, "contract-la-nobody-"+runID, auth.RoleViewer); !errors.Is(err, interfaces.ErrNotFound) {
			t.Errorf("a role change of an unknown id: %v, want ErrNotFound", err)
		}
		if err := b.UpdateUserRole(ctx, x, auth.RoleAdmin); err != nil {
			t.Fatalf("promote %s: %v", x, err)
		}
	})
}
