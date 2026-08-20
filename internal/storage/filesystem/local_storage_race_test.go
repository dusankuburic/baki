package filesystem

import (
	"context"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/interfaces"
)

// TestUserStore_ConcurrentReadWriteRaceFree is the regression gate for the
// shared-pointer escapes in the in-memory user store: readers previously got
// LIVE pointers into the guarded map while UpdateUserRole/Password/Profile
// mutated those same structs under the lock — a data race on every field read
// after return. The store now returns value copies; this test drives
// concurrent reads + writes under `go test -race`.
func TestUserStore_ConcurrentReadWriteRaceFree(t *testing.T) {
	lsb, err := NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	ctx := context.Background()
	if err := lsb.CreateUser(ctx, &interfaces.User{ID: "u1", Email: "u1@x.com", Role: auth.RoleMember}); err != nil {
		t.Fatalf("seed user: %v", err)
	}

	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				u, err := lsb.LoadUserByID(ctx, "u1")
				if err != nil {
					t.Error(err)
					return
				}
				_ = u.Role // read a field of the (previously shared) struct
				byEmail, err := lsb.LoadUserByEmail(ctx, "u1@x.com")
				if err != nil {
					t.Error(err)
					return
				}
				_ = byEmail.DisplayName
			}
		}()
	}
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 50 {
				_ = lsb.UpdateUserRole(ctx, "u1", auth.RoleAdmin)
				_ = lsb.UpdateUserProfile(ctx, "u1", "Ada", "")
			}
		}()
	}
	wg.Wait()
}

// TestUserStore_EmailIndexConsistency proves the usersByEmail index stays in
// lockstep with the users map across saves, email changes, and deletes.
func TestUserStore_EmailIndexConsistency(t *testing.T) {
	lsb, err := NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	ctx := context.Background()

	u := &interfaces.User{ID: "u1", Email: "Old@X.com", Role: auth.RoleMember}
	if err := lsb.CreateUser(ctx, u); err != nil {
		t.Fatalf("create: %v", err)
	}
	// Case-insensitive lookup.
	if _, err := lsb.LoadUserByEmail(ctx, "old@x.com"); err != nil {
		t.Errorf("lookup by original email: %v", err)
	}
	// Change the email — old index entry must go.
	u.Email = "new@x.com"
	if err := lsb.SaveUser(ctx, u); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := lsb.LoadUserByEmail(ctx, "old@x.com"); err == nil {
		t.Error("old email still resolves after email change (stale index)")
	}
	if got, err := lsb.LoadUserByEmail(ctx, "new@x.com"); err != nil || got.ID != "u1" {
		t.Errorf("new email lookup: %v, %v", got, err)
	}
	// The freed email must be claimable by another user.
	if err := lsb.CreateUser(ctx, &interfaces.User{ID: "u2", Email: "old@x.com"}); err != nil {
		t.Errorf("re-claim freed email: %v", err)
	}
	// Duplicate email under a different ID is still rejected.
	if err := lsb.SaveUser(ctx, &interfaces.User{ID: "u3", Email: "new@x.com"}); err == nil {
		t.Error("expected ErrEmailExists for duplicate email")
	}
	// Delete cleans the index.
	if err := lsb.DeleteUser(ctx, "u2"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := lsb.LoadUserByEmail(ctx, "old@x.com"); err == nil {
		t.Error("deleted user's email still resolves (stale index)")
	}
}

// TestOrgAndSharingStores_CopyOnRead proves readers get copies: mutating a
// returned org/collaborator must not alter what subsequent reads observe.
func TestOrgAndSharingStores_CopyOnRead(t *testing.T) {
	lsb, err := NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	ctx := context.Background()

	org := &interfaces.Organisation{ID: "o1", Name: "Org", Members: []interfaces.OrgMember{{UserID: "m1"}}}
	if err := lsb.SaveOrg(ctx, org); err != nil {
		t.Fatalf("save org: %v", err)
	}
	got, err := lsb.LoadOrg(ctx, "o1")
	if err != nil {
		t.Fatalf("load org: %v", err)
	}
	got.Name = "Mutated"
	got.Members[0].UserID = "hacked"
	fresh, _ := lsb.LoadOrg(ctx, "o1")
	if fresh.Name != "Org" || fresh.Members[0].UserID != "m1" {
		t.Errorf("caller mutation leaked into store: %+v", fresh)
	}

	c := &interfaces.Collaborator{UserID: "c1", Email: "c1@x.com", Permission: "viewer"}
	if err := lsb.AddCollaborator(ctx, "f1", c); err != nil {
		t.Fatalf("add collaborator: %v", err)
	}
	collabs, err := lsb.ListCollaborators(ctx, "f1")
	if err != nil || len(collabs) != 1 {
		t.Fatalf("list collaborators: %v %v", collabs, err)
	}
	collabs[0].Permission = "admin" // mutate the returned copy
	freshCollabs, _ := lsb.ListCollaborators(ctx, "f1")
	if freshCollabs[0].Permission != "viewer" {
		t.Errorf("collaborator mutation leaked into store: %+v", freshCollabs[0])
	}

	// MutateOrg's callback changes must be visible on the NEXT read and not
	// affect an org instance a reader already holds.
	held, _ := lsb.LoadOrg(ctx, "o1")
	if err := lsb.MutateOrg(ctx, "o1", func(o *interfaces.Organisation) error {
		o.Members = append(o.Members, interfaces.OrgMember{UserID: "m2"})
		return nil
	}); err != nil {
		t.Fatalf("mutate org: %v", err)
	}
	after, _ := lsb.LoadOrg(ctx, "o1")
	if len(after.Members) != 2 {
		t.Errorf("MutateOrg change not persisted: %+v", after.Members)
	}
	if len(held.Members) != 1 {
		t.Errorf("previously returned org was mutated in place: %+v", held.Members)
	}
}

// TestUserStore_UpdateTimestamps verifies the mutators still write through
// (the copy-on-read change must not have accidentally copied on WRITE too).
func TestUserStore_UpdateTimestamps(t *testing.T) {
	lsb, err := NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	ctx := context.Background()
	if err := lsb.CreateUser(ctx, &interfaces.User{ID: "u1", Email: "u1@x.com"}); err != nil {
		t.Fatalf("create: %v", err)
	}
	before := time.Now().UTC()
	if err := lsb.UpdateUserProfile(ctx, "u1", "Ada", "avatar.png"); err != nil {
		t.Fatalf("update profile: %v", err)
	}
	u, err := lsb.LoadUserByID(ctx, "u1")
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if u.DisplayName != "Ada" || u.AvatarURL != "avatar.png" {
		t.Errorf("profile update not persisted: %+v", u)
	}
	if u.UpdatedAt.Before(before) {
		t.Errorf("UpdatedAt not bumped: %v", u.UpdatedAt)
	}
}
