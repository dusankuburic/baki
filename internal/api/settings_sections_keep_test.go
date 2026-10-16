package api

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"pad-analyzer/internal/auth"
	storagedb "pad-analyzer/internal/storage/database"
	"pad-analyzer/internal/testutil"
	"pad-analyzer/internal/testutil/pgtest"
	"pad-core/models"
)

// settingsBody is doc as a client posts it, without the top-level keys in drop: what a client that predates a section
// (the profile, before the profile cycle) sends.
func settingsBody(t *testing.T, doc *models.AppSettings, drop ...string) map[string]any {
	t.Helper()
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	var body map[string]any
	if err := json.Unmarshal(data, &body); err != nil {
		t.Fatal(err)
	}
	for _, key := range drop {
		delete(body, key)
	}
	return body
}

// TestSettingsWrite_AbsentSectionKeepsItsStoredValue: POST /api/system/settings replaced the whole document, so a
// client that did not send a section (an older one, without profile) wiped it. A section the body leaves out now keeps
// its stored value, every section alike; one the body names is replaced, and an explicit {} or null clears it. With
// accounts on the fake and on Postgres, and without (the deployment's settings, as on the desktop).
func TestSettingsWrite_AbsentSectionKeepsItsStoredValue(t *testing.T) {
	cloud := func(t *testing.T, rt *Router) string {
		seedUserWithRole(t, rt, "sections-user", "sections-user@example.com", auth.RoleMember)
		return sessionBearer(t, rt, "sections-user", auth.RoleMember)
	}
	routers := []struct {
		name  string
		setup func(t *testing.T) (*Router, string)
	}{
		{"accounts, fake backend", func(t *testing.T) (*Router, string) {
			rt := newTestRouter(testutil.NewFakeBackend(), true)
			return rt, cloud(t, rt)
		}},
		{"accounts, postgres", func(t *testing.T) (*Router, string) {
			pg, err := storagedb.New(context.Background(), storagedb.DefaultConfig(pgtest.NewDatabase(t, "")))
			if err != nil {
				t.Fatalf("open the test database: %v", err)
			}
			t.Cleanup(func() { pg.Close() })
			rt := newTestRouter(pg, true)
			return rt, cloud(t, rt)
		}},
		{"no accounts", func(*testing.T) (*Router, string) {
			return newTestRouter(nil, false), "Bearer " + testToken
		}},
	}
	for _, tc := range routers {
		t.Run(tc.name, func(t *testing.T) {
			rt, bearer := tc.setup(t)
			post := func(body any) {
				t.Helper()
				checkStatus(t, doRequestWithAuth(t, rt, http.MethodPost, "/api/system/settings", bearer, body), http.StatusOK)
			}
			get := func() models.AppSettings {
				t.Helper()
				rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/system/settings", bearer, nil)
				checkStatus(t, rr, http.StatusOK)
				var got models.AppSettings
				decodeJSON(t, rr, &got)
				return got
			}
			profile := models.ProfileSettings{Bio: "Automates invoices", JobTitle: "RPA developer", Timezone: "Europe/Belgrade"}
			doc := models.DefaultSettings()
			doc.Profile = profile
			doc.Layout.SidebarWidth = 333
			post(doc)

			// An older client: everything but the profile, with a change of its own.
			doc.Appearance.Theme = "light"
			post(settingsBody(t, doc, "profile"))
			got := get()
			if got.Profile != profile {
				t.Errorf("a write without the profile section changed it to %+v, want %+v", got.Profile, profile)
			}
			if got.Appearance.Theme != "light" || got.Layout.SidebarWidth != 333 {
				t.Errorf("the named sections were not written: theme %q, sidebar %d", got.Appearance.Theme, got.Layout.SidebarWidth)
			}

			// A body naming one section leaves every other one as stored.
			post(map[string]any{"layout": map[string]any{"sidebarWidth": 290}})
			got = get()
			if got.Profile != profile || got.Appearance.Theme != "light" || got.Layout.SidebarWidth != 290 || got.AI.DailyBudget != doc.AI.DailyBudget {
				t.Errorf("after a layout-only write: profile %+v, theme %q, sidebar %d, budget %v", got.Profile, got.Appearance.Theme, got.Layout.SidebarWidth, got.AI.DailyBudget)
			}
			post(map[string]any{})
			if again := get(); again.Profile != profile || again.Layout.SidebarWidth != 290 {
				t.Errorf("an empty document changed the settings: profile %+v, sidebar %d", again.Profile, again.Layout.SidebarWidth)
			}

			// A named empty or null section clears it.
			post(map[string]any{"profile": map[string]any{}})
			if got := get(); got.Profile != (models.ProfileSettings{}) || got.Appearance.Theme != "light" {
				t.Errorf("after profile {}: profile %+v, theme %q; want the profile cleared and the rest kept", got.Profile, got.Appearance.Theme)
			}
			post(settingsBody(t, doc))
			post(map[string]any{"profile": nil})
			if got := get(); got.Profile != (models.ProfileSettings{}) || got.Appearance.Theme != "light" {
				t.Errorf("after profile null: profile %+v, theme %q; want the profile cleared and the rest kept", got.Profile, got.Appearance.Theme)
			}
		})
	}
}

// TestOrgSettingsWrite_AbsentSectionKeepsItsStoredValue: the org settings write keeps the sections its body leaves
// out, as the allow-list already was (TestOrgSettings_AllowedProvidersSurvivePartialWrites): the Rules panel posts
// only {analysis:{rules}}, which wiped the organisation's other sections.
func TestOrgSettingsWrite_AbsentSectionKeepsItsStoredValue(t *testing.T) {
	rt := newOrgSettingsRouter(t)
	orgID := seedOrg(t, rt, "acme", "alice")
	mirrorOrg(t, rt, orgID)
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")
	post := func(body any) {
		t.Helper()
		checkStatus(t, doRequestWithAuth(t, rt, http.MethodPost, "/api/system/settings/org/"+orgID, bearer, body), http.StatusOK)
	}

	post(map[string]any{"ai": map[string]any{"dailyBudget": 42, "showCostEstimates": true}, "parser": map[string]any{"maxFileSizeMB": 7}})
	post(map[string]any{"analysis": map[string]any{"rules": map[string]any{"dead-code": map[string]any{"enabled": false, "severity": "info"}}}})

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/system/settings/org/"+orgID, bearer, nil)
	checkStatus(t, rr, http.StatusOK)
	var got models.AppSettings
	decodeJSON(t, rr, &got)
	if got.AI.DailyBudget != 42 || !got.AI.ShowCostEstimates || got.Parser.MaxFileSizeMB != 7 {
		t.Errorf("a rules-only write wiped the other sections: ai %+v, parser %+v", got.AI, got.Parser)
	}
	if rule, ok := got.Analysis.Rules["dead-code"]; !ok || rule.Enabled {
		t.Errorf("the rules write was not stored: %+v", got.Analysis.Rules)
	}
}
