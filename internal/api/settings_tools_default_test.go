package api

import (
	"net/http"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// TestSettings_ToolsOffByDefaultRoundTrips: the AI setting behind the chat's tools default (R-frontend-6) is stored
// and read back. Absent (every stored document before it) reads as false, so tools stay on by default.
func TestSettings_ToolsOffByDefaultRoundTrips(t *testing.T) {
	routers := []struct {
		name  string
		setup func(t *testing.T) (*Router, string)
	}{
		{"accounts, fake backend", func(t *testing.T) (*Router, string) {
			rt := newTestRouter(testutil.NewFakeBackend(), true)
			seedUserWithRole(t, rt, "tools-user", "tools-user@example.com", auth.RoleMember)
			return rt, sessionBearer(t, rt, "tools-user", auth.RoleMember)
		}},
		{"no accounts", func(*testing.T) (*Router, string) {
			return newTestRouter(nil, false), "Bearer " + testToken
		}},
	}
	for _, tc := range routers {
		t.Run(tc.name, func(t *testing.T) {
			rt, bearer := tc.setup(t)
			toolsOff := func() any {
				t.Helper()
				rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/system/settings", bearer, nil)
				checkStatus(t, rr, http.StatusOK)
				var got struct {
					AI map[string]any `json:"ai"`
				}
				decodeJSON(t, rr, &got)
				return got.AI["toolsOffByDefault"]
			}
			if v := toolsOff(); v != nil && v != false {
				t.Fatalf("toolsOffByDefault before any write = %v, want absent or false", v)
			}

			body := settingsBody(t, models.DefaultSettings())
			body["ai"].(map[string]any)["toolsOffByDefault"] = true
			checkStatus(t, doRequestWithAuth(t, rt, http.MethodPost, "/api/system/settings", bearer, body), http.StatusOK)
			if v := toolsOff(); v != true {
				t.Errorf("toolsOffByDefault after writing true = %v, want true", v)
			}
		})
	}
}
