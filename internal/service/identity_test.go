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

// signupLinks is memLinks over users: a sign-up writes the account into users and the link here, as one backend
// stores both in one transaction.
type signupLinks struct {
	memLinks
	users *identityUsers
}

func (s signupLinks) SaveUserWithIdentityLink(ctx context.Context, u *storageif.User, l *storageif.IdentityLink) error {
	if _, err := s.LoadIdentityLink(ctx, l.Provider, l.Subject); err == nil {
		return errors.New("identity already linked")
	}
	if _, ok := s.users.byID[u.ID]; !ok {
		s.users.created = append(s.users.created, u)
	}
	if err := s.users.SaveUser(ctx, u); err != nil {
		return err
	}
	return s.SaveIdentityLink(ctx, l)
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
	svc := NewIdentityService(users, signupLinks{links, users})
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
	svc := NewIdentityService(users, signupLinks{links, users})

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
	svc := NewIdentityService(users, signupLinks{links, users})
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

// linkFailingBackend is a FakeBackend whose next identity-link write fails, as a crash or a lost connection between
// an SSO sign-up's account and its link would.
type linkFailingBackend struct {
	*testutil.FakeBackend
	failLink bool
}

func (b *linkFailingBackend) SaveIdentityLink(ctx context.Context, l *storageif.IdentityLink) error {
	if b.failLink {
		b.failLink = false
		return errors.New("simulated: connection lost before the identity link was written")
	}
	return b.FakeBackend.SaveIdentityLink(ctx, l)
}

// SaveUserWithIdentityLink fails whole, as the transaction it is does.
func (b *linkFailingBackend) SaveUserWithIdentityLink(ctx context.Context, u *storageif.User, l *storageif.IdentityLink) error {
	if b.failLink {
		b.failLink = false
		return errors.New("simulated: connection lost before the identity link was written")
	}
	return b.FakeBackend.SaveUserWithIdentityLink(ctx, u, l)
}

// An SSO sign-up whose link write fails leaves no account behind, so the retry signs up: the account used to be
// written first and alone, and the retry then found a verified account with the identity's email and no link, which
// it refuses to auto-link (a lockout until a password reset). The same holds for adopting an unverified account.
func TestIdentityService_FailedSignupLeavesNothingAndRetrySucceeds(t *testing.T) {
	ctx := context.Background()
	store := &linkFailingBackend{FakeBackend: testutil.NewFakeBackend(), failLink: true}
	svc := NewIdentityService(store, store)
	ident := func() *sso.Identity {
		return &sso.Identity{Subject: "sub-new", Email: "new@example.com", EmailVerified: true, DisplayName: "New"}
	}

	if _, err := svc.ResolveSSOUser(ctx, "entra", ident()); err == nil {
		t.Fatal("premise: the sign-up whose link write failed succeeded")
	}
	if u, err := store.LoadUserByEmail(ctx, "new@example.com"); err == nil {
		t.Errorf("the failed sign-up left account %+v without its identity link", u)
	}
	user, err := svc.ResolveSSOUser(ctx, "entra", ident())
	if err != nil {
		t.Fatalf("retrying the sign-up: %v", err)
	}
	if link, err := store.LoadIdentityLink(ctx, "entra", "sub-new"); err != nil || link.UserID != user.ID {
		t.Errorf("link after the retry = %+v, %v; want one to %s", link, err, user.ID)
	}

	// Adopting an unverified account: a failed link write leaves it as it was (unverified, its password intact), so
	// the retry adopts it.
	if err := store.SaveUser(ctx, &storageif.User{ID: "u-shadow", Email: "shadow@example.com", Password: "hash", Role: auth.RoleMember}); err != nil {
		t.Fatal(err)
	}
	store.failLink = true
	shadow := func() *sso.Identity {
		return &sso.Identity{Subject: "sub-shadow", Email: "shadow@example.com", EmailVerified: true}
	}
	if _, err := svc.ResolveSSOUser(ctx, "entra", shadow()); err == nil {
		t.Fatal("premise: the adoption whose link write failed succeeded")
	}
	if u, err := store.LoadUserByID(ctx, "u-shadow"); err != nil || u.EmailVerified || u.Password != "hash" {
		t.Errorf("after the failed adoption the account is %+v, %v; want it unchanged", u, err)
	}
	if got, err := svc.ResolveSSOUser(ctx, "entra", shadow()); err != nil || got.ID != "u-shadow" {
		t.Fatalf("retrying the adoption = %+v, %v; want u-shadow", got, err)
	}
	if u, err := store.LoadUserByID(ctx, "u-shadow"); err != nil || !u.EmailVerified || u.Password != "" {
		t.Errorf("after the adoption the account is %+v, %v; want verified without a password", u, err)
	}
}

// verifiedBetween is a FakeBackend whose owner verifies (and an admin renames) the account right after the SSO
// adoption read it.
type verifiedBetween struct {
	*testutil.FakeBackend
	verify bool
}

func (b *verifiedBetween) LoadUserByEmail(ctx context.Context, email string) (*storageif.User, error) {
	read, err := b.FakeBackend.LoadUserByEmail(ctx, email)
	if err != nil {
		return nil, err
	}
	now, _ := b.LoadUserByID(ctx, read.ID)
	now.EmailVerified = now.EmailVerified || b.verify
	now.Role, now.DisplayName = auth.RoleAdmin, "Renamed"
	if err := b.SaveUser(ctx, now); err != nil {
		return nil, err
	}
	return read, nil
}

// The adoption decides on the row it read, so it must not write that row back: an account its owner verified in
// between keeps its password and gets no link (the verified-account refusal), and one still unverified is adopted
// without reverting what changed in between. The returned user is the stored one.
func TestIdentityService_AdoptionWritesOnlyAStillUnverifiedAccount(t *testing.T) {
	ctx := context.Background()
	for _, verify := range []bool{true, false} {
		store := &verifiedBetween{FakeBackend: testutil.NewFakeBackend(), verify: verify}
		if err := store.SaveUser(ctx, &storageif.User{ID: "u-shadow", Email: "shadow@example.com", Password: "hash", Role: auth.RoleMember, DisplayName: "Shadow"}); err != nil {
			t.Fatal(err)
		}
		svc := NewIdentityService(store, store)
		got, err := svc.ResolveSSOUser(ctx, "entra", &sso.Identity{Subject: "sub-shadow", Email: "shadow@example.com", EmailVerified: true})
		stored, lerr := store.LoadUserByID(ctx, "u-shadow")
		if lerr != nil {
			t.Fatal(lerr)
		}
		if verify {
			if err == nil || err.Error() != "an account with this email already exists — link entra from your account settings" {
				t.Errorf("adopting an account verified in between = (%+v, %v); want the verified-account refusal", got, err)
			}
			if stored.Password != "hash" {
				t.Errorf("the refused adoption stripped the verified account's password: %+v", stored)
			}
			if link, err := store.LoadIdentityLink(ctx, "entra", "sub-shadow"); err == nil {
				t.Errorf("the refused adoption linked the identity: %+v", link)
			}
			continue
		}
		if err != nil {
			t.Fatalf("adopting a still unverified account: %v", err)
		}
		if !stored.EmailVerified || stored.Password != "" || stored.Role != auth.RoleAdmin || stored.DisplayName != "Renamed" {
			t.Errorf("the adopted account = %+v; want verified, no password, and the role and name set in between", stored)
		}
		if got.Role != auth.RoleAdmin || got.DisplayName != "Renamed" || !got.EmailVerified {
			t.Errorf("ResolveSSOUser returned %+v; want the stored account", got)
		}
	}
}
