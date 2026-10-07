package service

import (
	"context"
	"errors"
	"testing"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// collabFake is the fake backend with the collaborator writes and user lookups recorded.
type collabFake struct {
	*testutil.FakeBackend
	byID, byEmail map[string]*storageif.User
	addErr        error
	added         []*storageif.Collaborator
	removed       []string
}

func (f *collabFake) LoadUserByID(_ context.Context, id string) (*storageif.User, error) {
	if u, ok := f.byID[id]; ok {
		return u, nil
	}
	return nil, storageif.ErrNotFound
}

func (f *collabFake) LoadUserByEmail(_ context.Context, email string) (*storageif.User, error) {
	if u, ok := f.byEmail[email]; ok {
		return u, nil
	}
	return nil, storageif.ErrNotFound
}

func (f *collabFake) AddCollaborator(_ context.Context, _ string, c *storageif.Collaborator) error {
	if f.addErr != nil {
		return f.addErr
	}
	f.added = append(f.added, c)
	return nil
}

func (f *collabFake) RemoveCollaborator(_ context.Context, flowID, userID string) error {
	f.removed = append(f.removed, flowID+"/"+userID)
	return nil
}

func TestCollaboratorService_ResolveUserPrefersID(t *testing.T) {
	alice := &storageif.User{ID: "u-alice", Email: "alice@example.com"}
	bob := &storageif.User{ID: "u-bob", Email: "bob@example.com"}
	svc := NewCollaboratorService(&collabFake{
		FakeBackend: testutil.NewFakeBackend(),
		byID:        map[string]*storageif.User{alice.ID: alice},
		byEmail:     map[string]*storageif.User{bob.Email: bob},
	})
	ctx := context.Background()
	if u, err := svc.ResolveUser(ctx, alice.ID, bob.Email); err != nil || u != alice {
		t.Fatalf("by id: got %v, %v; want alice", u, err)
	}
	if u, err := svc.ResolveUser(ctx, "", bob.Email); err != nil || u != bob {
		t.Fatalf("by email: got %v, %v; want bob", u, err)
	}
	if _, err := svc.ResolveUser(ctx, "", "nobody@example.com"); !errors.Is(err, storageif.ErrNotFound) {
		t.Fatalf("unknown email: err = %v, want ErrNotFound", err)
	}
}

func TestCollaboratorService_AddBuildsTheGrant(t *testing.T) {
	fake := &collabFake{FakeBackend: testutil.NewFakeBackend()}
	svc := NewCollaboratorService(fake)
	u := &storageif.User{ID: "u1", Email: "u1@example.com"}
	c, err := svc.Add(context.Background(), "flow-1", u, "editor")
	if err != nil {
		t.Fatalf("Add: %v", err)
	}
	if c.UserID != "u1" || c.Email != "u1@example.com" || c.Permission != "editor" || c.GrantedAt.IsZero() {
		t.Fatalf("grant = %+v", c)
	}
	if len(fake.added) != 1 || fake.added[0] != c {
		t.Fatalf("stored %v, want the returned grant", fake.added)
	}
}

func TestCollaboratorService_AddMapsDuplicates(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want error
	}{
		{errors.New("pq: Duplicate key value"), ErrAlreadyCollaborator},
		{errors.New("user ALREADY has access"), ErrAlreadyCollaborator},
		{storageif.ErrNotFound, storageif.ErrNotFound},
	} {
		svc := NewCollaboratorService(&collabFake{FakeBackend: testutil.NewFakeBackend(), addErr: tc.err})
		_, err := svc.Add(context.Background(), "flow-1", &storageif.User{ID: "u1"}, "viewer")
		if !errors.Is(err, tc.want) {
			t.Errorf("store error %q: got %v, want %v", tc.err, err, tc.want)
		}
	}
	if ErrAlreadyCollaborator.Error() != "user is already a collaborator" {
		t.Errorf("message changed: %q", ErrAlreadyCollaborator)
	}
}

func TestCollaboratorService_PassThrough(t *testing.T) {
	fake := &collabFake{FakeBackend: testutil.NewFakeBackend()}
	svc := NewCollaboratorService(fake)
	ctx := context.Background()
	if _, err := svc.List(ctx, "flow-1", 10); err != nil {
		t.Fatalf("List: %v", err)
	}
	if got := fake.ListCollaboratorsCalls.Load(); got != 1 {
		t.Errorf("ListCollaborators calls = %d, want 1", got)
	}
	if err := svc.Update(ctx, "flow-1", "u1", "admin"); err != nil {
		t.Fatalf("Update: %v", err)
	}
	if err := svc.Remove(ctx, "flow-1", "u1"); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if len(fake.removed) != 1 || fake.removed[0] != "flow-1/u1" {
		t.Errorf("removed = %v", fake.removed)
	}
}

func TestNewCollaboratorService_NilBackend(t *testing.T) {
	var backend storageif.StorageBackend // the desktop: no store
	if NewCollaboratorService(backend) == nil {
		t.Fatal("construction over a nil backend must not fail")
	}
}
