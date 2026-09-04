package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

type orgRuleOut struct {
	ID      string `json:"id"`
	RuleID  string `json:"ruleId"`
	Enabled bool   `json:"enabled"`
	Config  struct {
		ID           string `json:"id"`
		Severity     string `json:"severity"`
		RawTypeMatch string `json:"rawTypeMatch"`
	} `json:"config"`
}

func ruleBody(id, match, severity string) map[string]any {
	return map[string]any{
		"config": map[string]any{
			"id": id, "name": id, "severity": severity,
			"category": "Style", "rawTypeMatch": match,
		},
	}
}

// TestOrgRules_AdminCRUD drives the per-org custom-rule lifecycle and the
// member/admin split. Reads are member-level (a developer needs to see what
// their org enforces); writes are admin-only, because analysis configuration
// decides what a team is told about its own flows.
func TestOrgRules_AdminCRUD(t *testing.T) {
	env := newChannelEnv(t)
	rt, admin, orgID := env.rt, env.admin, env.orgID
	peon := jwtBearer(t, rt, "peon", "peon@acme.io")

	// A non-member can neither read nor write.
	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/orgs/"+orgID+"/rules", peon, nil)
	if rr.Code != http.StatusForbidden && rr.Code != http.StatusNotFound {
		t.Fatalf("non-member list: status %d, want 403/404", rr.Code)
	}
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/orgs/"+orgID+"/rules", peon, ruleBody("x", "^SET$", "warning"))
	if rr.Code != http.StatusForbidden && rr.Code != http.StatusNotFound {
		t.Fatalf("non-member save: status %d, want 403/404", rr.Code)
	}

	// A config with no id is rejected — the id is what appears on findings.
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/orgs/"+orgID+"/rules", admin,
		map[string]any{"config": map[string]any{"name": "nameless", "rawTypeMatch": "^SET$"}})
	checkStatus(t, rr, http.StatusBadRequest)

	// An uncompilable matcher is rejected at the API, not stored and skipped
	// later — the author finds out now.
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/orgs/"+orgID+"/rules", admin,
		ruleBody("bad-regex", "^SET(", "warning"))
	checkStatus(t, rr, http.StatusBadRequest)

	// Valid create.
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/orgs/"+orgID+"/rules", admin,
		ruleBody("house-style", "^SET$", "warning"))
	checkStatus(t, rr, http.StatusOK)
	var created orgRuleOut
	if err := json.NewDecoder(rr.Body).Decode(&created); err != nil {
		t.Fatalf("decode created rule: %v", err)
	}
	if created.ID == "" || created.RuleID != "house-style" || !created.Enabled {
		t.Fatalf("unexpected created rule: %+v", created)
	}

	// A plain MEMBER can read the org's rules but still cannot write them.
	addOrgMember(t, rt, orgID, "peon", auth.RoleMember)
	rr = doRequestWithAuth(t, rt, http.MethodGet, "/api/orgs/"+orgID+"/rules", peon, nil)
	checkStatus(t, rr, http.StatusOK)
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/orgs/"+orgID+"/rules", peon,
		ruleBody("member-authored", "^IF$", "warning"))
	checkStatus(t, rr, http.StatusForbidden)

	// Admin lists it back with the config parsed, not as a blob.
	rr = doRequestWithAuth(t, rt, http.MethodGet, "/api/orgs/"+orgID+"/rules", admin, nil)
	checkStatus(t, rr, http.StatusOK)
	var listed []orgRuleOut
	if err := json.NewDecoder(rr.Body).Decode(&listed); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(listed) != 1 || listed[0].Config.RawTypeMatch != "^SET$" {
		t.Fatalf("list did not round-trip the config: %+v", listed)
	}

	// Re-saving the same rule id replaces rather than duplicating.
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/orgs/"+orgID+"/rules", admin,
		ruleBody("house-style", "^WAIT$", "error"))
	checkStatus(t, rr, http.StatusOK)
	rr = doRequestWithAuth(t, rt, http.MethodGet, "/api/orgs/"+orgID+"/rules", admin, nil)
	if err := json.NewDecoder(rr.Body).Decode(&listed); err != nil {
		t.Fatalf("decode list after re-save: %v", err)
	}
	if len(listed) != 1 {
		t.Errorf("re-saving the same rule id produced %d rows, want 1", len(listed))
	} else if listed[0].Config.Severity != "error" {
		t.Errorf("re-save did not replace the config: %+v", listed[0].Config)
	}

	// Delete.
	rr = doRequestWithAuth(t, rt, http.MethodDelete, "/api/orgs/"+orgID+"/rules/"+listed[0].ID, admin, nil)
	checkStatus(t, rr, http.StatusOK)
	rr = doRequestWithAuth(t, rt, http.MethodGet, "/api/orgs/"+orgID+"/rules", admin, nil)
	if err := json.NewDecoder(rr.Body).Decode(&listed); err != nil {
		t.Fatalf("decode list after delete: %v", err)
	}
	if len(listed) != 0 {
		t.Errorf("rule survived delete: %+v", listed)
	}
}

// TestDeploymentRuleConfig_RequiresSystemAdmin is the regression test for the
// multi-tenancy defect that motivated R4: /api/analysis/rule/{enabled,config}
// write the DEPLOYMENT-WIDE settings singleton, and used to accept any member —
// so one org's developer could disable a rule for every tenant in the
// deployment.
func TestDeploymentRuleConfig_RequiresSystemAdmin(t *testing.T) {
	backend := &testutil.FakeBackend{}
	rt := newTestRouter(backend, true)
	seedUserWithRole(t, rt, "member-1", "member@acme.io", auth.RoleMember)
	// NOT jwtBearer: that helper always issues an ADMIN token, so using it here
	// would assert nothing about the role gate.
	pair, err := rt.security.AuthMgr.Issue("member-1", "member@acme.io", auth.RoleMember)
	if err != nil {
		t.Fatalf("issue member jwt: %v", err)
	}
	member := "Bearer " + pair.AccessToken

	for _, tc := range []struct {
		path string
		body map[string]any
	}{
		{"/api/analysis/rule/enabled", map[string]any{"ruleId": "dead-code", "enabled": false}},
		{"/api/analysis/rule/config", map[string]any{"ruleId": "dead-code", "config": map[string]any{"enabled": false, "severity": "info"}}},
	} {
		t.Run(tc.path, func(t *testing.T) {
			rr := doRequestWithAuth(t, rt, http.MethodPost, tc.path, member, tc.body)
			if rr.Code != http.StatusForbidden {
				t.Errorf("a plain member got %d writing deployment-wide rule config at %s — want 403; this endpoint changes analysis for EVERY tenant", rr.Code, tc.path)
			}
		})
	}
}

// TestOrgSettings_InvalidatesRuleProfile covers a gap found by running the
// feature rather than by reading it: an admin could save an org rule override,
// re-analyze immediately, and see NO change, because the resolver kept serving
// its cached profile until the 30s TTL elapsed. The rules endpoints invalidated;
// the org-SETTINGS endpoint (which is where rule enable/severity actually live)
// did not.
//
// The symptom is indistinguishable from "the feature does not work", so the
// invalidation is worth pinning rather than trusting.
func TestOrgSettings_InvalidatesRuleProfile(t *testing.T) {
	env := newChannelEnv(t)
	rt, admin, orgID := env.rt, env.admin, env.orgID

	resolver := service.NewRuleProfileResolver(env.backend, &stubSettingsProvider{s: models.DefaultSettings()}, nil)
	rt.handlers.Sys.ruleProfiles = resolver

	ctx := context.Background()
	// Warm the cache for this org.
	if got := resolver.Resolve(ctx, orgID).Settings.Analysis.Rules["dead-code"]; got.Severity == "error" {
		t.Fatal("precondition: dead-code should not already be re-graded to error")
	}

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/system/settings/org/"+orgID, admin,
		map[string]any{"analysis": map[string]any{"rules": map[string]any{
			"dead-code": map[string]any{"enabled": true, "severity": "error"},
		}}})
	checkStatus(t, rr, http.StatusOK)

	// Immediately — not after the TTL.
	if got := resolver.Resolve(ctx, orgID).Settings.Analysis.Rules["dead-code"]; got.Severity != "error" {
		t.Errorf("the org's rule override was not visible right after the write (severity=%q) — the settings endpoint must invalidate the cached rule profile, or an admin sees no effect for up to the resolver TTL", got.Severity)
	}
}

// stubSettingsProvider is a deployment-settings stand-in for the test above.
type stubSettingsProvider struct{ s *models.AppSettings }

func (p *stubSettingsProvider) Get() *models.AppSettings          { return p.s }
func (p *stubSettingsProvider) Update(v models.AppSettings) error { p.s = &v; return nil }
func (p *stubSettingsProvider) AddRecentFile(string, int64) error { return nil }
func (p *stubSettingsProvider) RemoveRecentFile(string) error     { return nil }
func (p *stubSettingsProvider) ClearRecentFiles() error           { return nil }

// TestOrgRules_ExportIsBakicliShaped pins the CI-parity contract: the export is
// a .bakirc.json document whose field names and shapes MATCH cmd/bakicli's
// bakiConfig, so a pipeline gates on the same rules the org sees.
//
// The field names are asserted literally rather than through a shared type,
// because the CLI and the API do not share one — the whole risk is that the two
// drift apart and CI quietly enforces a different rule set. A rename on either
// side has to break this test.
func TestOrgRules_ExportIsBakicliShaped(t *testing.T) {
	env := newChannelEnv(t)
	rt, admin, orgID := env.rt, env.admin, env.orgID

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/orgs/"+orgID+"/rules", admin,
		ruleBody("acme-no-set", "^SET$", "error"))
	checkStatus(t, rr, http.StatusOK)
	// A paused rule must NOT reach CI.
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/orgs/"+orgID+"/rules", admin,
		map[string]any{"enabled": false, "config": map[string]any{
			"id": "acme-paused", "name": "paused", "severity": "warning",
			"category": "Style", "rawTypeMatch": "^WAIT$",
		}})
	checkStatus(t, rr, http.StatusOK)

	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/system/settings/org/"+orgID, admin,
		map[string]any{"analysis": map[string]any{"rules": map[string]any{
			"dead-code": map[string]any{"enabled": false, "severity": "info"},
		}}})
	checkStatus(t, rr, http.StatusOK)

	rr = doRequestWithAuth(t, rt, http.MethodGet, "/api/orgs/"+orgID+"/rules/export", admin, nil)
	checkStatus(t, rr, http.StatusOK)

	var raw map[string]json.RawMessage
	if err := json.NewDecoder(rr.Body).Decode(&raw); err != nil {
		t.Fatalf("decode export: %v", err)
	}
	for _, key := range []string{"ruleConfig", "customRulesInline"} {
		if _, ok := raw[key]; !ok {
			t.Errorf("export is missing %q — bakicli's bakiConfig reads that exact key", key)
		}
	}

	var export struct {
		RuleConfig map[string]struct {
			Enabled  *bool  `json:"enabled"`
			Severity string `json:"severity"`
		} `json:"ruleConfig"`
		CustomRulesInline []struct {
			ID           string `json:"id"`
			RawTypeMatch string `json:"rawTypeMatch"`
		} `json:"customRulesInline"`
	}
	rr2 := doRequestWithAuth(t, rt, http.MethodGet, "/api/orgs/"+orgID+"/rules/export", admin, nil)
	if err := json.NewDecoder(rr2.Body).Decode(&export); err != nil {
		t.Fatalf("decode typed export: %v", err)
	}

	rc, ok := export.RuleConfig["dead-code"]
	if !ok {
		t.Fatal("export dropped the org's rule-profile override")
	}
	if rc.Enabled == nil {
		t.Error("enabled must be emitted explicitly — bakicli treats an absent enabled as 'leave at default'")
	} else if *rc.Enabled {
		t.Error("export reported dead-code as enabled; the org disabled it")
	}
	if rc.Severity != "info" {
		t.Errorf("severity override lost in export: %q", rc.Severity)
	}

	if len(export.CustomRulesInline) != 1 {
		t.Fatalf("export carried %d custom rule(s), want 1 (the paused one must be excluded)", len(export.CustomRulesInline))
	}
	if export.CustomRulesInline[0].ID != "acme-no-set" || export.CustomRulesInline[0].RawTypeMatch != "^SET$" {
		t.Errorf("exported rule is not the org's: %+v", export.CustomRulesInline[0])
	}
}

// TestTestCustomRule_ReportsWhatItWouldMatch covers the question the validate
// endpoint cannot answer: not "does this rule compile" but "does it do
// anything". A regex that compiles and matches nothing is a policy the org
// believes is enforced and is not — the silent-no-op class R1-5's suppression
// inventory exists to surface.
func TestTestCustomRule_ReportsWhatItWouldMatch(t *testing.T) {
	backend := &testutil.FakeBackend{}
	rt := newTestRouter(backend, true)
	seedUserWithRole(t, rt, "dev", "dev@acme.io", auth.RoleMember)
	dev := jwtBearer(t, rt, "dev", "dev@acme.io")

	// A flow the caller owns, containing one SET.
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/library", dev, map[string]any{
		"name": "Main",
		"content": map[string]any{
			"name": "Main",
			"subflows": []map[string]any{{
				"name":   "Main",
				"blocks": []map[string]any{{"rawType": "SET", "name": "Set variable", "type": "action"}},
			}},
		},
	})
	checkStatus(t, rr, http.StatusCreated)
	var created struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&created); err != nil || created.ID == "" {
		t.Fatalf("create flow: %v (%s)", err, rr.Body.String())
	}

	post := func(rule map[string]any, flowID string) *httptest.ResponseRecorder {
		return doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/custom-rules/test", dev,
			map[string]any{"rule": rule, "flowId": flowID})
	}

	// A rule that matches.
	rr = post(map[string]any{"id": "hits", "name": "hits", "severity": "error", "rawTypeMatch": "^SET$"}, created.ID)
	checkStatus(t, rr, http.StatusOK)
	var res struct {
		Matches int `json:"matches"`
	}
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Matches == 0 {
		t.Error("a rule matching the flow's only action reported 0 matches")
	}

	// A rule that compiles but matches nothing — the case the author most needs
	// to see, and the one validate reports as perfectly fine.
	rr = post(map[string]any{"id": "misses", "name": "misses", "severity": "error", "rawTypeMatch": "^NOSUCHACTION$"}, created.ID)
	checkStatus(t, rr, http.StatusOK)
	if err := json.NewDecoder(rr.Body).Decode(&res); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if res.Matches != 0 {
		t.Errorf("a non-matching rule reported %d matches", res.Matches)
	}

	// An uncompilable rule is the author's error: 400, not 500.
	rr = post(map[string]any{"id": "bad", "name": "bad", "severity": "error", "rawTypeMatch": "^SET("}, created.ID)
	checkStatus(t, rr, http.StatusBadRequest)

	// Missing flowId is refused with guidance rather than silently testing
	// against nothing and reporting a reassuring zero.
	rr = post(map[string]any{"id": "x", "rawTypeMatch": "^SET$"}, "")
	checkStatus(t, rr, http.StatusBadRequest)

	// A flow the caller cannot read stays unreadable through this endpoint.
	seedOrgFlow(t, rt, "someone-elses-flow", "other-user", "")
	rr = post(map[string]any{"id": "x", "rawTypeMatch": "^SET$"}, "someone-elses-flow")
	if rr.Code == http.StatusOK {
		t.Error("rule-test analyzed a flow the caller has no access to")
	}
}
