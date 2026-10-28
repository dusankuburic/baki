package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"testing"

	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/audit"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/service"
)

// R-config-4's route: GET and PATCH /api/system/settings/org/{id}/sections/{section}. The write names fields of one
// section, which the server merges into the section as stored under the settings rows' lock; a write carrying a
// revision the section has moved past answers 409 SETTINGS_STALE.

type sectionState struct {
	Section  string         `json:"section"`
	Values   map[string]any `json:"values"`
	Revision string         `json:"revision"`
}

func getSection(t *testing.T, rt *Router, orgID, section, bearer string) sectionState {
	t.Helper()
	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/system/settings/org/"+orgID+"/sections/"+section, bearer, nil)
	checkStatus(t, rr, http.StatusOK)
	var st sectionState
	if err := json.Unmarshal(rr.Body.Bytes(), &st); err != nil {
		t.Fatalf("decode section: %v (%s)", err, rr.Body.String())
	}
	return st
}

func TestOrgSettingsSection_PatchMergesTheNamedFieldsAndRefusesAStaleRevision(t *testing.T) {
	rt := newOrgSettingsRouter(t)
	orgID := seedOrg(t, rt, "acme", "alice")
	mirrorOrg(t, rt, orgID)
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/system/settings/org/"+orgID, bearer,
		map[string]any{"ai": map[string]any{"allowedProviders": []string{"claude"}, "systemPromptSuffix": "old"}})
	checkStatus(t, rr, http.StatusOK)

	read := getSection(t, rt, orgID, "ai", bearer)
	if read.Section != "ai" || read.Revision == "" || read.Values["systemPromptSuffix"] != "old" {
		t.Fatalf("GET section = %+v, want the ai section with a revision", read)
	}

	path := "/api/system/settings/org/" + orgID + "/sections/ai"
	rr = doRequestWithAuth(t, rt, http.MethodPatch, path, bearer,
		map[string]any{"values": map[string]any{"systemPromptSuffix": "new"}, "ifRevision": read.Revision})
	checkStatus(t, rr, http.StatusOK)
	var written sectionState
	if err := json.Unmarshal(rr.Body.Bytes(), &written); err != nil {
		t.Fatal(err)
	}
	if written.Revision == read.Revision || written.Values["systemPromptSuffix"] != "new" {
		t.Errorf("PATCH answered %+v, want the section as written with a new revision", written)
	}
	if got := orgAllowedProviders(t, rt, orgID, bearer); !slices.Equal(got, []string{"claude"}) {
		t.Errorf("a write naming systemPromptSuffix changed the allowlist: %v, want [claude]", got)
	}

	// The second admin's write was based on the first read: refused, and nothing changes.
	rr = doRequestWithAuth(t, rt, http.MethodPatch, path, bearer,
		map[string]any{"values": map[string]any{"allowedProviders": []string{"openai"}}, "ifRevision": read.Revision})
	checkStatus(t, rr, http.StatusConflict)
	var env render.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil || env.Code != service.SettingsCodeStale {
		t.Errorf("stale write: body %s, want code %s", rr.Body.String(), service.SettingsCodeStale)
	}
	if got := orgAllowedProviders(t, rt, orgID, bearer); !slices.Equal(got, []string{"claude"}) {
		t.Errorf("a refused stale write changed the allowlist: %v", got)
	}

	// Reloaded, the same change lands; without a revision it lands too (the merge keeps the other fields).
	fresh := getSection(t, rt, orgID, "ai", bearer)
	rr = doRequestWithAuth(t, rt, http.MethodPatch, path, bearer,
		map[string]any{"values": map[string]any{"allowedProviders": []string{"openai"}}, "ifRevision": fresh.Revision})
	checkStatus(t, rr, http.StatusOK)
	rr = doRequestWithAuth(t, rt, http.MethodPatch, path, bearer, map[string]any{"values": map[string]any{"toolsOffByDefault": true}})
	checkStatus(t, rr, http.StatusOK)
	final := getSection(t, rt, orgID, "ai", bearer)
	if fmt.Sprint(final.Values["allowedProviders"]) != "[openai]" || final.Values["systemPromptSuffix"] != "new" || final.Values["toolsOffByDefault"] != true {
		t.Errorf("final ai section = %+v, want every write's field", final.Values)
	}
}

func TestOrgSettingsSection_Refusals(t *testing.T) {
	rt := newOrgSettingsRouter(t)
	orgID := seedOrg(t, rt, "acme", "alice")
	mirrorOrg(t, rt, orgID)
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")
	path := "/api/system/settings/org/" + orgID + "/sections/"

	rr := doRequestWithAuth(t, rt, http.MethodPatch, path+"ai", bearer,
		map[string]any{"values": map[string]any{"allowedProviders": []string{"claude", "nope"}}})
	checkStatus(t, rr, http.StatusBadRequest)
	var env render.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil || env.Code != service.ProviderCodeUnknown || fmt.Sprint(env.Details["providers"]) != "[nope]" {
		t.Errorf("unknown provider id: %s, want PROVIDER_UNKNOWN naming it", rr.Body.String())
	}
	for name, tc := range map[string]struct {
		section string
		body    any
	}{
		"unknown field":   {"ai", map[string]any{"values": map[string]any{"dailyBudget": 5}}},
		"unknown section": {"nope", map[string]any{"values": map[string]any{"x": 1}}},
		"recent files":    {"recentFiles", map[string]any{"values": map[string]any{"x": 1}}},
		"no values":       {"ai", map[string]any{}},
		"malformed body":  {"ai", "not an object"},
	} {
		rr := doRequestWithAuth(t, rt, http.MethodPatch, path+tc.section, bearer, tc.body)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("%s: %d %s, want 400", name, rr.Code, rr.Body.String())
		}
	}
}

func TestOrgSettingsSection_Authz(t *testing.T) {
	rt := newOrgSettingsRouter(t)
	orgID := seedOrg(t, rt, "acme", "alice")
	mirrorOrg(t, rt, orgID)
	addOrgMember(t, rt, orgID, "bob", auth.RoleMember)
	bob := jwtBearer(t, rt, "bob", "bob@example.com")
	carol := jwtBearer(t, rt, "carol", "carol@example.com")
	path := "/api/system/settings/org/" + orgID + "/sections/ai"

	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, path, bob, nil), http.StatusOK)
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, path, carol, nil), http.StatusForbidden)
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodPatch, path, bob, map[string]any{"values": map[string]any{"systemPromptSuffix": "x"}}), http.StatusForbidden)
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodPatch, path, carol, map[string]any{"values": map[string]any{"systemPromptSuffix": "x"}}), http.StatusForbidden)
}

// The audit row names the fields as the section knows them, whatever case the body used (encoding/json matches keys
// regardless of case, and so does the section write): a write of "AllowedProviders" is the org's AI policy and the
// row says what it became.
func TestOrgSettingsSection_AuditNamesTheCanonicalFields(t *testing.T) {
	rt := newOrgSettingsRouter(t)
	orgID := seedOrg(t, rt, "acme", "alice")
	mirrorOrg(t, rt, orgID)
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")
	ch := captureAuditEvents(t, 8)
	rr := doRequestWithAuth(t, rt, http.MethodPatch, "/api/system/settings/org/"+orgID+"/sections/ai", bearer,
		map[string]any{"values": map[string]any{"AllowedProviders": []string{"claude"}, "SYSTEMPROMPTSUFFIX": "x"}})
	checkStatus(t, rr, http.StatusOK)
	ev := nextAuditEvent(t, ch)
	if ev.Action != audit.ActionSettingsChange || ev.Meta["section"] != "ai" || ev.Meta["fields"] != "allowedProviders,systemPromptSuffix" ||
		ev.Meta["allowedProviders"] != "claude" {
		t.Errorf("audit row = %+v, want the canonical field names and the allowlist it became", ev)
	}
}

// A section write is capped like the whole-document write (maxSettingsBodyBytes), not only by the router's limit.
func TestOrgSettingsSection_BodyCappedLikeTheDocumentWrite(t *testing.T) {
	rt := newOrgSettingsRouter(t)
	orgID := seedOrg(t, rt, "acme", "alice")
	mirrorOrg(t, rt, orgID)
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")
	// The analysis section has no per-field cap: only the body limit stands between this write and the store.
	rules := map[string]any{}
	for i := 0; len(rules)*64 < maxSettingsBodyBytes+(64<<10); i++ {
		rules[fmt.Sprintf("rule-%06d-%s", i, strings.Repeat("x", 30))] = map[string]any{"enabled": false, "severity": "info"}
	}
	rr := doRequestWithAuth(t, rt, http.MethodPatch, "/api/system/settings/org/"+orgID+"/sections/analysis", bearer,
		map[string]any{"values": map[string]any{"rules": rules}})
	if rr.Code != http.StatusBadRequest {
		t.Errorf("a %d-rule body over %d bytes: %d, want 400", len(rules), maxSettingsBodyBytes, rr.Code)
	}
	// Under the cap it lands.
	rr = doRequestWithAuth(t, rt, http.MethodPatch, "/api/system/settings/org/"+orgID+"/sections/analysis", bearer,
		map[string]any{"values": map[string]any{"rules": map[string]any{"dead-code": map[string]any{"enabled": false, "severity": "info"}}}})
	checkStatus(t, rr, http.StatusOK)
}
