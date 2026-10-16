package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/collaboration"
	storagedb "pad-analyzer/internal/storage/database"
	"pad-analyzer/internal/testutil"
	"pad-analyzer/internal/testutil/pgtest"
)

// TestOrgMemberRoutes_NonMemberIsNotFound: removing a user who is not a member of the organisation, or changing their
// role, is a 404 like an unknown organisation, not a 500, on every org store the service runs over (the in-memory
// one, the fake backend, Postgres), and changes nothing.
func TestOrgMemberRoutes_NonMemberIsNotFound(t *testing.T) {
	stores := []struct {
		name  string
		store func(t *testing.T) collaboration.OrgStore
	}{
		{"memory", func(*testing.T) collaboration.OrgStore { return collaboration.NewMemOrgStore() }},
		{"fake backend", func(*testing.T) collaboration.OrgStore { return testutil.NewFakeBackend() }},
		{"postgres", func(t *testing.T) collaboration.OrgStore {
			pg, err := storagedb.New(context.Background(), storagedb.DefaultConfig(pgtest.NewDatabase(t, "")))
			if err != nil {
				t.Fatalf("open the test database: %v", err)
			}
			t.Cleanup(func() { pg.Close() })
			return pg
		}},
	}
	for _, tc := range stores {
		t.Run(tc.name, func(t *testing.T) {
			h, orgSvc := newOrgRosterRouter(t, tc.store(t), "u-admin")
			ctx := context.Background()
			org, err := orgSvc.Create(ctx, "Acme", "u-admin")
			if err != nil {
				t.Fatalf("create org: %v", err)
			}
			if err := orgSvc.AddMember(ctx, org.ID, "u-member", auth.RoleMember); err != nil {
				t.Fatalf("add member: %v", err)
			}
			send := func(method, path, body string) *httptest.ResponseRecorder {
				rr := httptest.NewRecorder()
				req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
				req.Header.Set("Content-Type", "application/json")
				h.ServeHTTP(rr, req)
				return rr
			}

			base := "/api/orgs/" + org.ID + "/members/u-stranger"
			if rr := send(http.MethodDelete, base, ""); rr.Code != http.StatusNotFound {
				t.Errorf("removing a non-member: %d %s, want 404", rr.Code, rr.Body.String())
			}
			if rr := send(http.MethodPut, base+"/role", `{"role":"admin"}`); rr.Code != http.StatusNotFound {
				t.Errorf("changing a non-member's role: %d %s, want 404", rr.Code, rr.Body.String())
			}
			members, err := orgSvc.ListMembers(ctx, org.ID)
			if err != nil {
				t.Fatalf("list members: %v", err)
			}
			ids := make([]string, 0, len(members))
			for _, m := range members {
				ids = append(ids, m.UserID+"="+string(m.Role))
			}
			slices.Sort(ids)
			if want := []string{"u-admin=admin", "u-member=member"}; !slices.Equal(ids, want) {
				t.Errorf("roster after the refused changes = %v, want %v", ids, want)
			}

			// The member routes' other answers are unchanged: a member is removed, the last admin is a conflict
			// whether removed or demoted.
			if rr := send(http.MethodPut, "/api/orgs/"+org.ID+"/members/u-admin/role", `{"role":"member"}`); rr.Code != http.StatusConflict {
				t.Errorf("demoting the last admin: %d %s, want 409", rr.Code, rr.Body.String())
			}
			if rr := send(http.MethodDelete, "/api/orgs/"+org.ID+"/members/u-member", ""); rr.Code != http.StatusOK {
				t.Errorf("removing a member: %d %s, want 200", rr.Code, rr.Body.String())
			}
		})
	}
}
