package service

import (
	"context"
	"errors"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/sso"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// identityUsers is a FakeBackend whose user methods read and write a map.
type identityUsers struct {
	*testutil.FakeBackend
	byID        map[string]*storageif.User
	created     []*storageif.User
	invalidated []string
}

func newIdentityUsers(users ...*storageif.User) *identityUsers {
	s := &identityUsers{FakeBackend: testutil.NewFakeBackend(), byID: map[string]*storageif.User{}}
	for _, u := range users {
		s.byID[u.ID] = u
	}
	return s
}

func (s *identityUsers) LoadUserByID(_ context.Context, id string) (*storageif.User, error) {
	if u, ok := s.byID[id]; ok {
		cp := *u
		return &cp, nil
	}
	return nil, storageif.ErrNotFound
}

func (s *identityUsers) LoadUserByEmail(_ context.Context, email string) (*storageif.User, error) {
	for _, u := range s.byID {
		if u.Email == email {
			cp := *u
			return &cp, nil
		}
	}
	return nil, storageif.ErrNotFound
}

func (s *identityUsers) SaveUser(_ context.Context, u *storageif.User) error {
	cp := *u
	s.byID[u.ID] = &cp
	return nil
}

func (s *identityUsers) CreateUser(_ context.Context, u *storageif.User) error {
	s.created = append(s.created, u)
	return s.SaveUser(context.Background(), u)
}

func (s *identityUsers) InvalidateUserTokens(_ context.Context, userID string, _ ...string) error {
	s.invalidated = append(s.invalidated, userID)
	return nil
}

// memLinks is an in-memory IdentityLinkStore.
type memLinks map[string]*storageif.IdentityLink

func (m memLinks) SaveIdentityLink(_ context.Context, l *storageif.IdentityLink) error {
	cp := *l
	m[l.Provider+"\x00"+l.Subject] = &cp
	return nil
}

func (m memLinks) LoadIdentityLink(_ context.Context, provider, subject string) (*storageif.IdentityLink, error) {
	if l, ok := m[provider+"\x00"+subject]; ok {
		return l, nil
	}
	return nil, storageif.ErrNotFound
}

func (m memLinks) ListIdentityLinks(_ context.Context, userID string) ([]*storageif.IdentityLink, error) {
	var out []*storageif.IdentityLink
	for _, l := range m {
		if l.UserID == userID {
			out = append(out, l)
		}
	}
	return out, nil
}

func (m memLinks) DeleteIdentityLink(_ context.Context, userID, provider string) error {
	for k, l := range m {
		if l.UserID == userID && l.Provider == provider {
			delete(m, k)
		}
	}
	return nil
}

func TestIdentityService_Available(t *testing.T) {
	var nilSvc *IdentityService
	if nilSvc.Available() || NewIdentityService(nil, nil).Available() {
		t.Error("no link store must not be available")
	}
}

// First login provisions an SSO-only member; the next login follows the link.
func TestIdentityService_ResolveProvisionsThenFollowsLink(t *testing.T) {
	users, links := newIdentityUsers(), memLinks{}
	svc := NewIdentityService(users, links)
	ident := &sso.Identity{Subject: "sub-1", Email: "New@Example.com", EmailVerified: true, DisplayName: "New"}
	user, err := svc.ResolveSSOUser(context.Background(), "entra", ident)
	if err != nil {
		t.Fatalf("provision: %v", err)
	}
	if user.Email != "new@example.com" || user.Password != "" || !user.EmailVerified || user.Role != auth.RoleMember {
		t.Errorf("provisioned %+v", user)
	}
	again, err := svc.ResolveSSOUser(context.Background(), "entra", &sso.Identity{Subject: "sub-1"})
	if err != nil || again.ID != user.ID {
		t.Errorf("linked login = %+v, %v; want %s", again, err, user.ID)
	}
	if len(users.created) != 1 {
		t.Errorf("created %d users, want 1", len(users.created))
	}
}

// An unverified local account with the asserted email is taken over: its password is cleared and its reset/verify
// tokens invalidated. A verified one is never auto-linked, nor is an unverified IdP email.
func TestIdentityService_ResolveExistingAccount(t *testing.T) {
	ctx := context.Background()
	shadow := &storageif.User{ID: "u-shadow", Email: "shadow@example.com", Password: "hash"}
	owner := &storageif.User{ID: "u-owner", Email: "owner@example.com", Password: "hash", EmailVerified: true}
	users, links := newIdentityUsers(shadow, owner), memLinks{}
	svc := NewIdentityService(users, links)

	got, err := svc.ResolveSSOUser(ctx, "entra", &sso.Identity{Subject: "s1", Email: "shadow@example.com", EmailVerified: true})
	if err != nil || got.ID != "u-shadow" {
		t.Fatalf("shadow takeover = %+v, %v", got, err)
	}
	if saved := users.byID["u-shadow"]; saved.Password != "" || !saved.EmailVerified {
		t.Errorf("shadow not hardened: %+v", saved)
	}
	if len(users.invalidated) != 1 || users.invalidated[0] != "u-shadow" {
		t.Errorf("invalidated %v, want the shadow's tokens", users.invalidated)
	}

	if _, err := svc.ResolveSSOUser(ctx, "entra", &sso.Identity{Subject: "s2", Email: "owner@example.com", EmailVerified: true}); err == nil {
		t.Error("a verified account must not be auto-linked")
	}
	if _, err := svc.ResolveSSOUser(ctx, "entra", &sso.Identity{Subject: "s3", Email: "x@example.com"}); err == nil {
		t.Error("an unverified IdP email must not provision")
	}
	if _, ok := links["entra\x00s2"]; ok {
		t.Error("a refused identity was linked")
	}
}

func TestIdentityService_LinkAndUnlink(t *testing.T) {
	ctx := context.Background()
	users := newIdentityUsers(&storageif.User{ID: "u1", Password: "hash"}, &storageif.User{ID: "u2"})
	links := memLinks{}
	svc := NewIdentityService(users, links)
	ident := &sso.Identity{Subject: "sub", Email: "A@Example.com"}

	if err := svc.Link(ctx, "entra", "u1", ident); err != nil {
		t.Fatalf("link: %v", err)
	}
	if err := svc.Link(ctx, "entra", "u1", ident); err != nil {
		t.Errorf("re-link the same account: %v", err)
	}
	if err := svc.Link(ctx, "entra", "u2", ident); err == nil {
		t.Error("an identity linked to another account must be refused")
	}
	if l, _ := svc.Links(ctx, "u1"); len(l) != 1 || l[0].Email != "a@example.com" {
		t.Errorf("links = %+v", l)
	}

	_ = links.SaveIdentityLink(ctx, &storageif.IdentityLink{Provider: "entra", Subject: "sub2", UserID: "u2"})
	if err := svc.Unlink(ctx, "u2", "entra"); !errors.Is(err, ErrUnlinkOnlySignIn) {
		t.Errorf("unlink without a password: %v, want ErrUnlinkOnlySignIn", err)
	}
	if err := svc.Unlink(ctx, "u1", "entra"); err != nil {
		t.Errorf("unlink: %v", err)
	}
	if err := svc.Unlink(ctx, "nobody", "entra"); !errors.Is(err, storageif.ErrNotFound) {
		t.Errorf("unknown user: %v, want ErrNotFound", err)
	}
}
