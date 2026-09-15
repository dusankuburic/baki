package api

import (
	"net/http"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/storage/filesystem"
)

// TestOrgCreate_IsSelfServeForAnyAuthenticatedUser pins a contract that a stale
// doc comment misrepresented for long enough to look like a security hole.
//
// The Swagger block on handleOrgCreate read "Only available to system admins"
// and declared a 403 the handler cannot produce. An audit reasonably flagged
// that as a missing role gate — but the code was right and the comment was
// wrong: SettingsModal renders OrganizationsPanel for EVERY authenticated cloud
// user and its create form is ungated (the isAdmin flag in that panel is
// ORG-scoped membership, not a system role). Adding RequireRole(admin) would
// have broken org creation for every non-admin in the shipped product.
//
// So this test exists to make the decision explicit and load-bearing in BOTH
// directions: if someone later decides creation really should be admin-only,
// this fails and forces them to update the UI and the docs in the same change,
// instead of silently 403-ing a button the product still shows.
func TestOrgCreate_IsSelfServeForAnyAuthenticatedUser(t *testing.T) {
	backend, err := filesystem.NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	rt := newTestRouter(backend, true) // cloud / JWT mode

	for _, role := range []auth.Role{auth.RoleMember, auth.RoleViewer} {
		t.Run(string(role), func(t *testing.T) {
			pair, err := rt.security.AuthMgr.Issue("user-"+string(role), string(role)+"@example.test", role)
			if err != nil {
				t.Fatalf("issue token: %v", err)
			}
			rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/orgs",
				"Bearer "+pair.AccessToken, map[string]any{"name": "Self Serve " + string(role)})

			if rr.Code == http.StatusForbidden {
				t.Fatalf("role %s got 403 creating an org. If org creation is now admin-only that "+
					"may be correct — but the settings UI still shows the create form to this user, "+
					"so the UI and the API docs must change in the same commit.", role)
			}
			if rr.Code != http.StatusOK {
				t.Fatalf("role %s: status %d, body %s", role, rr.Code, rr.Body.String())
			}
		})
	}
}
