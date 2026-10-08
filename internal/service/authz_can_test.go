package service

import (
	"context"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// canBackend is a FakeBackend with the collaborator and user reads the flow checks make. It counts every read a
// decision can make, so a test can show one taken before any of them.
type canBackend struct {
	*testutil.FakeBackend
	collabs map[string][]*storageif.Collaborator
	users   map[string]*storageif.User
	headers atomic.Int64
	reads   atomic.Int64
}

func (b *canBackend) LoadFlowHeader(ctx context.Context, id string) (*storageif.FlowDocument, error) {
	b.headers.Add(1)
	b.reads.Add(1)
	return b.FakeBackend.LoadFlowHeader(ctx, id)
}

func (b *canBackend) ListCollaborators(_ context.Context, flowID string, _ int) ([]*storageif.Collaborator, error) {
	b.reads.Add(1)
	return b.collabs[flowID], nil
}

func (b *canBackend) LoadUserByID(_ context.Context, id string) (*storageif.User, error) {
	b.reads.Add(1)
	if u, ok := b.users[id]; ok {
		return u, nil
	}
	return nil, storageif.ErrNotFound
}

// canOrgStore counts organisation reads, the roster and the membership row alike; fail, when set, fails every read.
type canOrgStore struct {
	collaboration.OrgStore
	loads atomic.Int64
	fail  error
}

func (s *canOrgStore) LoadOrg(ctx context.Context, id string) (*storageif.Organisation, error) {
	s.loads.Add(1)
	if s.fail != nil {
		return nil, s.fail
	}
	return s.OrgStore.LoadOrg(ctx, id)
}

func (s *canOrgStore) OrgMemberRole(ctx context.Context, orgID, userID string) (auth.Role, bool, error) {
	s.loads.Add(1)
	if s.fail != nil {
		return "", false, s.fail
	}
	return s.OrgStore.OrgMemberRole(ctx, orgID, userID)
}

type canFixture struct {
	authz   *AuthzService
	backend *canBackend
	orgs    *canOrgStore
	orgID   string
}

func (f *canFixture) reads() int64 { return f.backend.reads.Load() + f.orgs.loads.Load() }

// newCanFixture seeds one organisation (admin u-orgadmin; members u-owner, u-orgmember; viewer u-orgviewer), the
// flows f-personal (u-owner's, shared with u-cviewer, u-ceditor and u-cadmin), f-org (u-owner's, in the org),
// f-admin (u-orgadmin's, in the org) and f-ownerless, and the global admin u-root.
func newCanFixture(t *testing.T) *canFixture {
	t.Helper()
	ctx := context.Background()
	orgs := &canOrgStore{OrgStore: collaboration.NewMemOrgStore()}
	orgSvc := collaboration.NewOrgService(orgs)
	org, err := orgSvc.Create(ctx, "Acme", "u-orgadmin")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	for user, role := range map[string]auth.Role{"u-owner": auth.RoleMember, "u-orgmember": auth.RoleMember, "u-orgviewer": auth.RoleViewer} {
		if err := orgSvc.AddMember(ctx, org.ID, user, role); err != nil {
			t.Fatalf("add %s: %v", user, err)
		}
	}
	b := &canBackend{
		FakeBackend: testutil.NewFakeBackend(),
		collabs: map[string][]*storageif.Collaborator{"f-personal": {
			{UserID: "u-cviewer", Permission: "viewer"},
			{UserID: "u-ceditor", Permission: "editor"},
			{UserID: "u-cadmin", Permission: "admin"},
		}},
		users: map[string]*storageif.User{"u-root": {ID: "u-root", Role: auth.RoleAdmin}},
	}
	for _, f := range []*storageif.FlowDocument{
		{ID: "f-personal", Name: "personal", OwnerID: "u-owner"},
		{ID: "f-org", Name: "org", OwnerID: "u-owner", OrganizationID: org.ID},
		{ID: "f-admin", Name: "admin", OwnerID: "u-orgadmin", OrganizationID: org.ID},
		{ID: "f-ownerless", Name: "legacy"},
	} {
		if err := b.SaveFlow(ctx, f); err != nil {
			t.Fatalf("seed %s: %v", f.ID, err)
		}
	}
	b.reads.Store(0)
	b.headers.Store(0)
	orgs.loads.Store(0)
	return &canFixture{authz: NewAuthzService(b, orgSvc), backend: b, orgs: orgs, orgID: org.ID}
}

func userActor(userID string, role auth.Role) *auth.Actor {
	return &auth.Actor{Kind: auth.ActorUser, UserID: userID, Role: role}
}

func tokenActor(userID string, role auth.Role, scopes ...string) *auth.Actor {
	return &auth.Actor{Kind: auth.ActorToken, UserID: userID, Role: role, TokenID: "tok", Scopes: scopes}
}

var (
	flowActions     = []Action{FlowRead, FlowEdit, FlowShare, FlowDelete, ChatUse}
	orgActions      = []Action{OrgRead, OrgUse, OrgWrite, OrgAdmin}
	instanceActions = []Action{InstanceAdmin, InstanceWrite}
	allActions      = slices.Concat(flowActions, orgActions, instanceActions)
)

// TestCan_TokenScopes pins the scopes each action accepts. The caller, u-orgadmin with the global admin role,
// passes every role check on f-admin, the org and the instance, so only the actor's kind and scopes decide; a
// scope refusal is ErrScopeDenied and is decided before any read.
func TestCan_TokenScopes(t *testing.T) {
	if len(allActions) != len(actionSpecs)-1 {
		t.Fatalf("%d actions pinned here, %d defined: add the new one to the lists above", len(allActions), len(actionSpecs)-1)
	}
	writeActions := []Action{FlowRead, FlowEdit, FlowShare, FlowDelete, OrgRead, OrgUse, OrgWrite, OrgAdmin, InstanceWrite}
	cases := []struct {
		name    string
		actor   *auth.Actor
		allowed []Action
	}{
		{"session", userActor("u-orgadmin", auth.RoleAdmin), allActions},
		{"unscoped token", tokenActor("u-orgadmin", auth.RoleAdmin), allActions},
		{"read token", tokenActor("u-orgadmin", auth.RoleAdmin, auth.ScopeRead), []Action{FlowRead, OrgRead, OrgUse}},
		{"write token", tokenActor("u-orgadmin", auth.RoleAdmin, auth.ScopeWrite), writeActions},
		{"read+write token", tokenActor("u-orgadmin", auth.RoleAdmin, auth.ScopeRead, auth.ScopeWrite), writeActions},
		{"chat token", tokenActor("u-orgadmin", auth.RoleAdmin, auth.ScopeChat), []Action{ChatUse}},
		{"chat+write token", tokenActor("u-orgadmin", auth.RoleAdmin, auth.ScopeChat, auth.ScopeWrite), append([]Action{ChatUse}, writeActions...)},
		{"admin token", tokenActor("u-orgadmin", auth.RoleAdmin, auth.ScopeAdmin), []Action{InstanceAdmin}},
		{"system", auth.SystemActor("test"), allActions},
	}
	f := newCanFixture(t)
	resources := map[resourceKind]Resource{resFlow: FlowByID("f-admin"), resOrg: OrgByID(f.orgID), resInstance: Instance()}
	for _, tc := range cases {
		for _, action := range allActions {
			before := f.reads()
			err := f.authz.Can(context.Background(), tc.actor, action, resources[actionSpecs[action].on])
			if slices.Contains(tc.allowed, action) {
				if err != nil {
					t.Errorf("%s %s: refused: %v", tc.name, action, err)
				}
				continue
			}
			if !errors.Is(err, ErrScopeDenied) || !errors.Is(err, ErrPermissionDenied) {
				t.Errorf("%s %s: err = %v, want ErrScopeDenied", tc.name, action, err)
			}
			if n := f.reads() - before; n != 0 {
				t.Errorf("%s %s: %d reads before the scope refusal, want 0", tc.name, action, n)
			}
			if attempt := CanAttempt(tc.actor, action); !errors.Is(attempt, ErrScopeDenied) {
				t.Errorf("%s %s: CanAttempt = %v, want ErrScopeDenied like Can", tc.name, action, attempt)
			}
		}
	}

	// Nobody is refused outright, before any read, and not as a scope matter.
	before := f.reads()
	for _, action := range allActions {
		err := f.authz.Can(context.Background(), nil, action, resources[actionSpecs[action].on])
		if !errors.Is(err, ErrPermissionDenied) || errors.Is(err, ErrScopeDenied) {
			t.Errorf("nil actor %s: err = %v, want ErrPermissionDenied", action, err)
		}
	}
	if n := f.reads() - before; n != 0 {
		t.Errorf("nil actor: %d reads, want 0", n)
	}
}

// TestCan_FlowAccess covers who may do what to a flow: ownership, the organisation's roles, collaborator grants,
// the instance admin's read of ownerless flows, and a missing flow, by id and by a loaded header alike.
func TestCan_FlowAccess(t *testing.T) {
	cases := []struct {
		flow, user string
		role       auth.Role
		allowed    []Action
	}{
		{"f-personal", "u-owner", auth.RoleMember, flowActions},
		{"f-personal", "u-cviewer", auth.RoleMember, []Action{FlowRead, ChatUse}},
		{"f-personal", "u-ceditor", auth.RoleMember, []Action{FlowRead, FlowEdit, ChatUse}},
		// An admin-tier collaborator manages sharing but never deletes.
		{"f-personal", "u-cadmin", auth.RoleMember, []Action{FlowRead, FlowEdit, FlowShare, ChatUse}},
		{"f-personal", "u-orgadmin", auth.RoleMember, nil},
		{"f-personal", "u-outsider", auth.RoleMember, nil},
		// The global admin role grants nothing on someone's flow.
		{"f-personal", "u-root", auth.RoleAdmin, nil},
		{"f-org", "u-owner", auth.RoleMember, flowActions},
		{"f-org", "u-orgadmin", auth.RoleMember, flowActions},
		{"f-org", "u-orgmember", auth.RoleMember, []Action{FlowRead, FlowEdit, ChatUse}},
		{"f-org", "u-orgviewer", auth.RoleMember, []Action{FlowRead, ChatUse}},
		{"f-org", "u-cadmin", auth.RoleMember, nil},
		{"f-org", "u-outsider", auth.RoleMember, nil},
		// Ownerless legacy flows: readable by an instance admin (by the stored user role), writable by nobody.
		{"f-ownerless", "u-root", auth.RoleAdmin, []Action{FlowRead, ChatUse}},
		{"f-ownerless", "u-owner", auth.RoleMember, nil},
	}
	f := newCanFixture(t)
	ctx := context.Background()
	for _, tc := range cases {
		actor := userActor(tc.user, tc.role)
		header, err := f.backend.FakeBackend.LoadFlowHeader(ctx, tc.flow)
		if err != nil {
			t.Fatalf("load %s: %v", tc.flow, err)
		}
		for _, action := range flowActions {
			for _, res := range []Resource{FlowByID(tc.flow), FlowHeader(header)} {
				err := f.authz.Can(ctx, actor, action, res)
				if slices.Contains(tc.allowed, action) {
					if err != nil {
						t.Errorf("%s %s on %s: refused: %v", tc.user, action, res, err)
					}
					continue
				}
				if !errors.Is(err, ErrPermissionDenied) || errors.Is(err, ErrScopeDenied) {
					t.Errorf("%s %s on %s: err = %v, want a role refusal", tc.user, action, res, err)
				}
			}
		}
	}

	// A missing flow is the store's not-found (404), never a denial (403).
	for _, action := range flowActions {
		if err := f.authz.Can(ctx, userActor("u-owner", auth.RoleMember), action, FlowByID("f-missing")); !errors.Is(err, storageif.ErrNotFound) {
			t.Errorf("%s on a missing flow: err = %v, want ErrNotFound", action, err)
		}
	}
}

// TestCan_FlowHeaderSkipsTheRead: a loaded header saves the header read; by id, Can reads it once.
func TestCan_FlowHeaderSkipsTheRead(t *testing.T) {
	f := newCanFixture(t)
	ctx := context.Background()
	header, _ := f.backend.FakeBackend.LoadFlowHeader(ctx, "f-personal")
	owner := userActor("u-owner", auth.RoleMember)
	if err := f.authz.Can(ctx, owner, FlowEdit, FlowHeader(header)); err != nil || f.backend.headers.Load() != 0 {
		t.Fatalf("by header: err=%v header reads=%d, want nil and 0", err, f.backend.headers.Load())
	}
	if err := f.authz.Can(ctx, owner, FlowEdit, FlowByID("f-personal")); err != nil || f.backend.headers.Load() != 1 {
		t.Fatalf("by id: err=%v header reads=%d, want nil and 1", err, f.backend.headers.Load())
	}
}

// TestCan_OrgAccess covers the org actions by role, and an unknown organisation: a denial for the member actions
// (no existence oracle), ErrNotFound for OrgAdmin (the admin routes' 404).
func TestCan_OrgAccess(t *testing.T) {
	cases := []struct {
		user    string
		allowed []Action
	}{
		{"u-orgadmin", orgActions},
		{"u-orgmember", []Action{OrgRead, OrgUse, OrgWrite}},
		{"u-orgviewer", []Action{OrgRead}},
		{"u-outsider", nil},
		{"u-root", nil}, // the global admin role is no membership
	}
	f := newCanFixture(t)
	ctx := context.Background()
	for _, tc := range cases {
		actor := userActor(tc.user, auth.RoleAdmin)
		for _, action := range orgActions {
			err := f.authz.Can(ctx, actor, action, OrgByID(f.orgID))
			org, authErr := f.authz.AuthorizeOrg(ctx, actor, action, f.orgID)
			if slices.Contains(tc.allowed, action) {
				if err != nil || authErr != nil || org == nil || org.ID != f.orgID {
					t.Errorf("%s %s: Can=%v AuthorizeOrg=(%v, %v), want allowed", tc.user, action, err, org, authErr)
				}
				continue
			}
			for _, e := range []error{err, authErr} {
				if !errors.Is(e, ErrPermissionDenied) || errors.Is(e, ErrScopeDenied) || errors.Is(e, ErrNotFound) {
					t.Errorf("%s %s: err = %v, want a role refusal", tc.user, action, e)
				}
			}
		}
	}

	for _, action := range orgActions {
		err := f.authz.Can(ctx, userActor("u-orgadmin", auth.RoleMember), action, OrgByID("o-missing"))
		_, authErr := f.authz.AuthorizeOrg(ctx, userActor("u-orgadmin", auth.RoleMember), action, "o-missing")
		for _, e := range []error{err, authErr} {
			if action == OrgAdmin {
				if !errors.Is(e, ErrNotFound) || errors.Is(e, ErrPermissionDenied) {
					t.Errorf("OrgAdmin on an unknown org: err = %v, want ErrNotFound", e)
				}
			} else if !errors.Is(e, ErrPermissionDenied) || errors.Is(e, ErrNotFound) {
				t.Errorf("%s on an unknown org: err = %v, want ErrPermissionDenied", action, e)
			}
		}
	}
}

// TestCan_OrgReadFailure: a failed membership read is a denial for the member actions and the error itself for
// OrgAdmin, whose routes tell it from a refusal.
func TestCan_OrgReadFailure(t *testing.T) {
	f := newCanFixture(t)
	boom := errors.New("connection reset")
	f.orgs.fail = boom
	actor := userActor("u-orgadmin", auth.RoleMember)
	if err := f.authz.Can(context.Background(), actor, OrgRead, OrgByID(f.orgID)); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("OrgRead: err = %v, want ErrPermissionDenied", err)
	}
	if err := f.authz.Can(context.Background(), actor, OrgAdmin, OrgByID(f.orgID)); !errors.Is(err, boom) || errors.Is(err, ErrPermissionDenied) {
		t.Errorf("OrgAdmin: err = %v, want the read error", err)
	}
	if _, err := f.authz.AuthorizeOrg(context.Background(), actor, OrgAdmin, f.orgID); !errors.Is(err, boom) {
		t.Errorf("AuthorizeOrg OrgAdmin: err = %v, want the read error", err)
	}
}

// TestCan_OrgDecisionsShareTheMemo: with the request memo, several org decisions cost one membership read.
func TestCan_OrgDecisionsShareTheMemo(t *testing.T) {
	f := newCanFixture(t)
	ctx := WithFlowAuthzMemo(context.Background())
	actor := userActor("u-orgmember", auth.RoleMember)
	_ = f.authz.Can(ctx, actor, OrgRead, OrgByID(f.orgID))
	_ = f.authz.Can(ctx, actor, OrgWrite, OrgByID(f.orgID))
	_ = f.authz.Can(ctx, actor, OrgAdmin, OrgByID(f.orgID))
	if n := f.orgs.loads.Load(); n != 1 {
		t.Fatalf("membership reads = %d, want 1", n)
	}
}

// pointLookupOrgStore counts the membership point lookups apart from canOrgStore's roster reads, and answers an
// unknown organisation as Postgres does.
type pointLookupOrgStore struct {
	*canOrgStore
	points atomic.Int64
}

func (s *pointLookupOrgStore) OrgMemberRole(ctx context.Context, orgID, userID string) (auth.Role, bool, error) {
	s.points.Add(1)
	org, err := s.OrgStore.LoadOrg(ctx, orgID) // the inner store: not a counted roster read
	if err != nil {
		return "", false, storageif.ErrNotFound // as Postgres answers an unknown organisation
	}
	for _, m := range org.Members {
		if m.UserID == userID {
			return m.Role, true, nil
		}
	}
	return "", false, nil
}

// TestCan_OrgAdminUsesThePointLookup: an admin decision reads the caller's membership row, never the roster, and
// still tells an unknown organisation (404) from a refusal (403).
func TestCan_OrgAdminUsesThePointLookup(t *testing.T) {
	ctx := context.Background()
	store := &pointLookupOrgStore{canOrgStore: &canOrgStore{OrgStore: collaboration.NewMemOrgStore()}}
	orgSvc := collaboration.NewOrgService(store)
	org, err := orgSvc.Create(ctx, "Acme", "u-admin")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(ctx, org.ID, "u-member", auth.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	store.loads.Store(0)
	a := NewAuthzService(testutil.NewFakeBackend(), orgSvc)

	if err := a.Can(ctx, userActor("u-admin", auth.RoleMember), OrgAdmin, OrgByID(org.ID)); err != nil {
		t.Fatalf("admin refused: %v", err)
	}
	if p, l := store.points.Load(), store.loads.Load(); p != 1 || l != 0 {
		t.Fatalf("point lookups=%d roster reads=%d, want 1 and 0", p, l)
	}
	if err := a.Can(ctx, userActor("u-member", auth.RoleMember), OrgAdmin, OrgByID(org.ID)); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("member: err = %v, want ErrPermissionDenied", err)
	}
	if err := a.Can(ctx, userActor("u-admin", auth.RoleMember), OrgAdmin, OrgByID("o-missing")); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown org: err = %v, want ErrNotFound", err)
	}
}

// TestAuthorizeOrg_OneRosterRead: the member routes render the organisation they check, so AuthorizeOrg decides
// on the roster it returns instead of reading the membership as well.
func TestAuthorizeOrg_OneRosterRead(t *testing.T) {
	f := newCanFixture(t)
	org, err := f.authz.AuthorizeOrg(context.Background(), userActor("u-orgviewer", auth.RoleMember), OrgRead, f.orgID)
	if err != nil || len(org.Members) != 4 {
		t.Fatalf("AuthorizeOrg = (%v, %v), want the organisation with its 4 members", org, err)
	}
	if n := f.orgs.loads.Load(); n != 1 {
		t.Fatalf("organisation reads = %d, want 1", n)
	}
}

func TestCan_InstanceRoles(t *testing.T) {
	cases := []struct {
		role    auth.Role
		allowed []Action
	}{
		{auth.RoleAdmin, instanceActions},
		{auth.RoleMember, []Action{InstanceWrite}},
		{auth.RoleViewer, nil},
		{auth.RoleGuest, nil},
		{"", nil},
	}
	f := newCanFixture(t)
	for _, tc := range cases {
		for _, action := range instanceActions {
			err := f.authz.Can(context.Background(), userActor("u-someone", tc.role), action, Instance())
			if slices.Contains(tc.allowed, action) != (err == nil) {
				t.Errorf("role %q %s: err = %v", tc.role, action, err)
			}
			if err != nil && (!errors.Is(err, ErrPermissionDenied) || errors.Is(err, ErrScopeDenied)) {
				t.Errorf("role %q %s: err = %v, want a role refusal", tc.role, action, err)
			}
		}
	}
	if f.reads() != 0 {
		t.Errorf("instance decisions read storage %d times; the actor's role decides", f.reads())
	}
}

// TestCan_RefusesInvalidInput: a nil or malformed actor, an unknown action, a missing or mismatched resource are
// refused (ErrPermissionDenied, not a scope refusal) without a read.
func TestCan_RefusesInvalidInput(t *testing.T) {
	f := newCanFixture(t)
	owner := userActor("u-owner", auth.RoleAdmin)
	cases := []struct {
		name   string
		actor  *auth.Actor
		action Action
		res    Resource
	}{
		{"nil actor", nil, FlowRead, FlowByID("f-personal")},
		{"nil actor, scoped action", nil, ChatUse, FlowByID("f-personal")},
		{"actor without a kind", &auth.Actor{UserID: "u-owner", Role: auth.RoleAdmin}, FlowRead, FlowByID("f-personal")},
		{"user without an id", userActor("", auth.RoleAdmin), InstanceWrite, Instance()},
		{"token without an id", tokenActor("", auth.RoleAdmin), FlowRead, FlowByID("f-ownerless")},
		{"zero action", owner, Action(0), FlowByID("f-personal")},
		{"unknown action", owner, Action(200), FlowByID("f-personal")},
		{"zero resource", owner, FlowRead, Resource{}},
		{"nil header", owner, FlowRead, FlowHeader(nil)},
		{"header without an id", owner, FlowRead, FlowHeader(&storageif.FlowDocument{OwnerID: "u-owner"})},
		{"flow action on an org", owner, FlowEdit, OrgByID(f.orgID)},
		{"org action on the instance", owner, OrgRead, Instance()},
		{"instance action on a flow", owner, InstanceAdmin, FlowByID("f-personal")},
		{"system actor, mismatched resource", auth.SystemActor("job"), FlowEdit, Instance()},
	}
	for _, tc := range cases {
		err := f.authz.Can(context.Background(), tc.actor, tc.action, tc.res)
		if !errors.Is(err, ErrPermissionDenied) || errors.Is(err, ErrScopeDenied) {
			t.Errorf("%s: err = %v, want ErrPermissionDenied", tc.name, err)
		}
	}
	if f.reads() != 0 {
		t.Errorf("invalid input cost %d reads, want 0", f.reads())
	}
}

// TestCan_SystemActor: a background job may do everything, decided without a read, a missing flow included.
func TestCan_SystemActor(t *testing.T) {
	f := newCanFixture(t)
	sys := auth.SystemActor("retention")
	for _, action := range allActions {
		var res Resource
		switch actionSpecs[action].on {
		case resFlow:
			res = FlowByID("f-missing")
		case resOrg:
			res = OrgByID("o-missing")
		default:
			res = Instance()
		}
		if err := f.authz.Can(context.Background(), sys, action, res); err != nil {
			t.Errorf("system %s: %v", action, err)
		}
	}
	if f.reads() != 0 {
		t.Errorf("system decisions cost %d reads, want 0", f.reads())
	}
	h, err := f.authz.AuthorizeFlow(context.Background(), sys, FlowDelete, "f-admin")
	if err != nil || h == nil || h.ID != "f-admin" {
		t.Errorf("system AuthorizeFlow = (%v, %v), want the header", h, err)
	}
}

func TestAuthorizeFlow(t *testing.T) {
	f := newCanFixture(t)
	ctx := context.Background()
	h, err := f.authz.AuthorizeFlow(ctx, userActor("u-ceditor", auth.RoleMember), FlowEdit, "f-personal")
	if err != nil || h == nil || h.ID != "f-personal" || h.OwnerID != "u-owner" {
		t.Fatalf("editor: AuthorizeFlow = (%+v, %v), want the header", h, err)
	}
	if n := f.backend.headers.Load(); n != 1 {
		t.Fatalf("header reads = %d, want 1", n)
	}
	if h, err := f.authz.AuthorizeFlow(ctx, userActor("u-cviewer", auth.RoleMember), FlowEdit, "f-personal"); h != nil || !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("viewer editing: (%v, %v), want ErrPermissionDenied and no header", h, err)
	}
	if _, err := f.authz.AuthorizeFlow(ctx, userActor("u-owner", auth.RoleMember), FlowRead, "f-missing"); !errors.Is(err, storageif.ErrNotFound) {
		t.Errorf("missing flow: err = %v, want ErrNotFound", err)
	}
	before := f.reads()
	if _, err := f.authz.AuthorizeFlow(ctx, tokenActor("u-owner", auth.RoleMember, auth.ScopeChat), FlowEdit, "f-personal"); !errors.Is(err, ErrScopeDenied) {
		t.Errorf("chat token editing: err = %v, want ErrScopeDenied", err)
	}
	if _, err := f.authz.AuthorizeFlow(ctx, userActor("u-owner", auth.RoleMember), OrgRead, "f-personal"); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("org action through AuthorizeFlow: err = %v, want ErrPermissionDenied", err)
	}
	if n := f.reads() - before; n != 0 {
		t.Errorf("refusals before the read cost %d reads, want 0", n)
	}
}

// TestCan_LocalMode: without storage (the desktop app) the flow and org checks allow any valid actor, as the
// existing checks do; the actor's own checks and the global role still apply.
func TestCan_LocalMode(t *testing.T) {
	a := NewAuthzService(nil, nil)
	ctx := context.Background()
	local := auth.LocalActor("local-user")
	for _, action := range allActions {
		var res Resource
		switch actionSpecs[action].on {
		case resFlow:
			res = FlowByID("anything")
		case resOrg:
			res = OrgByID("anything")
		default:
			res = Instance()
		}
		if err := a.Can(ctx, local, action, res); err != nil {
			t.Errorf("local user %s: %v", action, err)
		}
		if err := a.Can(ctx, nil, action, res); !errors.Is(err, ErrPermissionDenied) {
			t.Errorf("nil actor %s: err = %v, want ErrPermissionDenied", action, err)
		}
	}
	if err := a.Can(ctx, tokenActor("local-user", auth.RoleAdmin, auth.ScopeChat), FlowEdit, FlowByID("f")); !errors.Is(err, ErrScopeDenied) {
		t.Errorf("chat token editing: err = %v, want ErrScopeDenied", err)
	}
	if err := a.Can(ctx, userActor("local-user", auth.RoleMember), InstanceAdmin, Instance()); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("member role administering: err = %v, want ErrPermissionDenied", err)
	}
	if _, err := a.AuthorizeFlow(ctx, local, FlowRead, "f"); !errors.Is(err, ErrPermissionDenied) {
		t.Errorf("AuthorizeFlow without storage: err = %v, want ErrPermissionDenied (callers handle local mode)", err)
	}
}

// ErrScopeDenied is a 403 like any denial, and its message names what the token lacks.
func TestErrScopeDenied(t *testing.T) {
	err := CanAttempt(tokenActor("u1", auth.RoleMember, auth.ScopeChat), FlowRead)
	if !errors.Is(err, ErrScopeDenied) || !errors.Is(err, ErrPermissionDenied) {
		t.Fatalf("err = %v, want ErrScopeDenied wrapping ErrPermissionDenied", err)
	}
	if msg := err.Error(); !strings.Contains(msg, "flow.read") || !strings.Contains(msg, "read or write") {
		t.Errorf("message %q does not name the action and the scopes it needs", msg)
	}
	if errors.Is(ErrPermissionDenied, ErrScopeDenied) {
		t.Error("a plain denial must not read as a scope refusal")
	}
}
