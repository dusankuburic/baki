package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/go-chi/chi/v5"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	"pad-analyzer/internal/config"
	mail "pad-analyzer/internal/mail"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// rosterCountingOrgStore counts the roster loads (LoadOrg) an org route
// costs. Without a point membership lookup, OrgService.MemberRole falls back
// to LoadOrg too, so every membership question is one roster load here.
type rosterCountingOrgStore struct {
	collaboration.OrgStore
	loadOrg atomic.Int64
}

func (s *rosterCountingOrgStore) LoadOrg(ctx context.Context, id string) (*storageif.Organisation, error) {
	s.loadOrg.Add(1)
	return s.OrgStore.LoadOrg(ctx, id)
}

// pointLookupOrgStore adds the Postgres backend's point membership lookup
// (storageif.OrgMembershipReader) to rosterCountingOrgStore, counted
// separately, so a route can be shown to use it instead of the roster.
type pointLookupOrgStore struct {
	*rosterCountingOrgStore
	memberRole atomic.Int64
}

func (s *pointLookupOrgStore) OrgMemberRole(ctx context.Context, orgID, userID string) (auth.Role, bool, error) {
	s.memberRole.Add(1)
	org, err := s.OrgStore.LoadOrg(ctx, orgID) // the inner store: not a counted roster load
	if err != nil {
		return "", false, err
	}
	for _, m := range org.Members {
		if m.UserID == userID {
			return m.Role, true, nil
		}
	}
	return "", false, nil
}

// newOrgRosterRouter serves the org member and invite routes for the
// (local-mode) caller callerID over the given org store.
func newOrgRosterRouter(t *testing.T, store collaboration.OrgStore, callerID string) (http.Handler, *collaboration.OrgService) {
	t.Helper()
	orgSvc := collaboration.NewOrgService(store)
	h := NewOrgHandler(orgSvc, testutil.NewFakeBackend(), nil, &SecurityConfig{LocalUserID: callerID},
		mail.NewService(config.EmailConfig{}, config.ModeLocal), nil, nil)
	r := chi.NewRouter()
	r.Get("/api/orgs/{id}/members", h.handleOrgMemberList)
	r.Get("/api/orgs/{id}/invites", h.handleOrgInviteList)
	return r, orgSvc
}

func getOrgRoute(h http.Handler, path string) *httptest.ResponseRecorder {
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, path, nil))
	return rr
}

// TestOrgMemberList_OneRosterLoad: GET /api/orgs/{id}/members loads the
// organisation once — the membership check's load is the roster listed. It
// used to load it three times (IsMember, Get, ListMembers).
func TestOrgMemberList_OneRosterLoad(t *testing.T) {
	store := &rosterCountingOrgStore{OrgStore: collaboration.NewMemOrgStore()}
	h, orgSvc := newOrgRosterRouter(t, store, "u-member")
	org, err := orgSvc.Create(context.Background(), "Acme", "u-admin")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	if err := orgSvc.AddMember(context.Background(), org.ID, "u-member", auth.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	store.loadOrg.Store(0)

	rr := getOrgRoute(h, "/api/orgs/"+org.ID+"/members")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	var members []storageif.OrgMember
	if err := json.Unmarshal(rr.Body.Bytes(), &members); err != nil {
		t.Fatalf("decode members: %v", err)
	}
	if len(members) != 2 {
		t.Fatalf("got %d members, want 2 (admin + member): %s", len(members), rr.Body.String())
	}
	if n := store.loadOrg.Load(); n != 1 {
		t.Fatalf("GET members loaded the organisation %d times; want exactly 1", n)
	}
}

// TestOrgMemberList_NonMemberAndUnknownOrgForbidden: the single-load check
// keeps the member routes' answers — 403 for a non-member and for an unknown
// organisation alike (no existence oracle).
func TestOrgMemberList_NonMemberAndUnknownOrgForbidden(t *testing.T) {
	store := &rosterCountingOrgStore{OrgStore: collaboration.NewMemOrgStore()}
	h, orgSvc := newOrgRosterRouter(t, store, "u-stranger")
	org, err := orgSvc.Create(context.Background(), "Acme", "u-admin")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	for _, id := range []string{org.ID, "no-such-org"} {
		if rr := getOrgRoute(h, "/api/orgs/"+id+"/members"); rr.Code != http.StatusForbidden {
			t.Errorf("org %q: status = %d, want 403", id, rr.Code)
		}
	}
}

// TestOrgAdminDecision_PointLookupNotRoster: an admin-only route that does
// not render the organisation (the invite list) decides on the caller's
// membership row — the point lookup a Postgres store offers — and never
// loads the roster.
func TestOrgAdminDecision_PointLookupNotRoster(t *testing.T) {
	store := &pointLookupOrgStore{rosterCountingOrgStore: &rosterCountingOrgStore{OrgStore: collaboration.NewMemOrgStore()}}
	h, orgSvc := newOrgRosterRouter(t, store, "u-admin")
	org, err := orgSvc.Create(context.Background(), "Acme", "u-admin")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	store.loadOrg.Store(0)

	rr := getOrgRoute(h, "/api/orgs/"+org.ID+"/invites")
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, body: %s", rr.Code, rr.Body.String())
	}
	if n := store.memberRole.Load(); n != 1 {
		t.Fatalf("membership point lookups = %d, want 1", n)
	}
	if n := store.loadOrg.Load(); n != 0 {
		t.Fatalf("the admin decision loaded the roster %d times; want 0", n)
	}

	// Same answers as the roster-loading check: a member who is not an admin
	// is refused with 403, an unknown organisation with 404.
	memberH, _ := newOrgRosterRouter(t, store, "u-member")
	if err := orgSvc.AddMember(context.Background(), org.ID, "u-member", auth.RoleMember); err != nil {
		t.Fatalf("add member: %v", err)
	}
	if rr := getOrgRoute(memberH, "/api/orgs/"+org.ID+"/invites"); rr.Code != http.StatusForbidden {
		t.Errorf("member: status = %d, want 403", rr.Code)
	}
	if rr := getOrgRoute(h, "/api/orgs/no-such-org/invites"); rr.Code != http.StatusNotFound {
		t.Errorf("unknown org: status = %d, want 404", rr.Code)
	}
}
