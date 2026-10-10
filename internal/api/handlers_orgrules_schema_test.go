package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// wantFieldError checks a 400 BAD_REQUEST whose details name the field and the reason.
func wantFieldError(t *testing.T, rr *httptest.ResponseRecorder, field, reason string) {
	t.Helper()
	checkStatus(t, rr, http.StatusBadRequest)
	e := errorBody(t, rr)
	if e.Code != "BAD_REQUEST" || e.Details["field"] != field || e.Details["reason"] != reason {
		t.Errorf("error = %+v, want BAD_REQUEST with field %q reason %q", e, field, reason)
	}
}

func seedOrgRule(t *testing.T, b *testutil.FakeBackend, orgID, rowID, ruleID, config string) {
	t.Helper()
	if err := b.SaveOrgCustomRule(context.Background(), &storageif.OrgCustomRule{
		ID: rowID, OrgID: orgID, RuleID: ruleID, Config: json.RawMessage(config), Enabled: true}); err != nil {
		t.Fatalf("seed %s: %v", rowID, err)
	}
}

type listedOrgRule struct {
	ID            string         `json:"id"`
	RuleID        string         `json:"ruleId"`
	Enabled       bool           `json:"enabled"`
	Config        map[string]any `json:"config"`
	Problem       string         `json:"problem"`
	ProblemReason string         `json:"problemReason"`
}

func listOrgRulesByID(t *testing.T, env *channelEnv) map[string]listedOrgRule {
	t.Helper()
	rr := doRequestWithAuth(t, env.rt, http.MethodGet, "/api/orgs/"+env.orgID+"/rules", env.admin, nil)
	checkStatus(t, rr, http.StatusOK)
	var rows []listedOrgRule
	decodeJSON(t, rr, &rows)
	out := make(map[string]listedOrgRule, len(rows))
	for _, r := range rows {
		out[r.RuleID] = r
	}
	return out
}

// A save is checked strictly: the request's own unknown field, the rule's unknown field, a kind this build cannot
// run, a built-in rule's id and an invalid value are each a 400 BAD_REQUEST naming the field and the reason.
func TestOrgRules_SaveIsStrict(t *testing.T) {
	env := newChannelEnv(t)
	post := func(body any) *httptest.ResponseRecorder {
		return doRequestWithAuth(t, env.rt, http.MethodPost, "/api/orgs/"+env.orgID+"/rules", env.admin, body)
	}
	cfg := func(extra map[string]any) map[string]any {
		c := map[string]any{"id": "house", "name": "House", "severity": "warning", "rawTypeMatch": "^SET$"}
		for k, v := range extra {
			c[k] = v
		}
		return map[string]any{"config": c}
	}

	wantFieldError(t, post(cfg(map[string]any{"nameMatches": "typo"})), "config.nameMatches", "UNKNOWN_FIELD")
	wantFieldError(t, post(cfg(map[string]any{"kind": "flow"})), "config.kind", "UNKNOWN_KIND")
	wantFieldError(t, post(cfg(map[string]any{"schemaVersion": 2})), "config.schemaVersion", "UNSUPPORTED_VERSION")
	wantFieldError(t, post(cfg(map[string]any{"id": "dead-code"})), "config.id", "RESERVED_ID")
	wantFieldError(t, post(cfg(map[string]any{"id": ""})), "config.id", "MISSING_ID")
	rr := post(cfg(map[string]any{"rawTypeMatch": "^SET("}))
	raw := rr.Body.String()
	wantFieldError(t, rr, "config.rawTypeMatch", "INVALID_PATTERN")
	if !strings.Contains(raw, `"message":"invalid rule: error parsing regexp`) {
		t.Errorf("an invalid pattern's message = %s, want today's wording after \"invalid rule: \"", raw)
	}

	body := cfg(nil)
	body["extra"] = true
	rr = post(body)
	checkStatus(t, rr, http.StatusBadRequest)
	if e := errorBody(t, rr); e.Code != "BAD_REQUEST" || !strings.Contains(e.Message, "extra") {
		t.Errorf("an unknown request field: %+v", e)
	}

	if rows := listOrgRulesByID(t, env); len(rows) != 0 {
		t.Errorf("refused saves stored %d rule(s)", len(rows))
	}
	rr = post(cfg(map[string]any{"autoFix": "remove-block", "kind": "block", "schemaVersion": 1}))
	checkStatus(t, rr, http.StatusOK)
}

// The list echoes each config normalized (kind and schemaVersion spelled out, never the unknown fields), and a
// rule the analysis skips stays listed with its problem.
func TestOrgRules_ListShowsProblems(t *testing.T) {
	env := newChannelEnv(t)
	rr := doRequestWithAuth(t, env.rt, http.MethodPost, "/api/orgs/"+env.orgID+"/rules", env.admin, ruleBody("clean", "^SET$", "warning"))
	checkStatus(t, rr, http.StatusOK)
	var saved listedOrgRule
	decodeJSON(t, rr, &saved)
	if saved.Config["kind"] != "block" || saved.Config["schemaVersion"] != 1.0 || saved.Problem != "" {
		t.Errorf("save echo = %+v", saved)
	}
	seedOrgRule(t, env.backend, env.orgID, "row-fwd", "fwd", `{"id":"fwd","rawTypeMatch":"^SET$","futureField":1}`)
	seedOrgRule(t, env.backend, env.orgID, "row-future", "future", `{"kind":"flow","id":"future","when":{"lacks":"Retry"}}`)
	seedOrgRule(t, env.backend, env.orgID, "row-sev", "old-sev", `{"id":"old-sev","severity":"critical","rawTypeMatch":"^SET$"}`)
	seedOrgRule(t, env.backend, env.orgID, "row-garbled", "garbled", `{"id":"garbled","severity":3}`)

	rows := listOrgRulesByID(t, env)
	if len(rows) != 5 {
		t.Fatalf("listed %d rules, want 5", len(rows))
	}
	for id, want := range map[string]string{"clean": "", "fwd": "", "future": "UNKNOWN_KIND", "old-sev": "INVALID_SEVERITY", "garbled": "INVALID_JSON"} {
		r := rows[id]
		if r.ProblemReason != want || (want == "") != (r.Problem == "") {
			t.Errorf("%s: problem %q (%s), want reason %q", id, r.Problem, r.ProblemReason, want)
		}
	}
	if _, ok := rows["fwd"].Config["futureField"]; ok {
		t.Error("the echo carries an unknown field: the editor would send it back and the strict save refuse it")
	}
	if c := rows["garbled"].Config; c["id"] != "garbled" || c["kind"] != "block" {
		t.Errorf("an undecodable row's echo = %v, want its rule id", c)
	}
	if rows["future"].Config["kind"] != "flow" {
		t.Errorf("a newer release's rule is echoed as kind %v", rows["future"].Config["kind"])
	}
}

// Pausing re-validates nothing (a rule that no longer compiles can be paused), editing a listed rule sends back an
// echo the strict save accepts, and a rule's id cannot change (409 instead of the old 500).
func TestOrgRules_ToggleEchoAndRename(t *testing.T) {
	env := newChannelEnv(t)
	post := func(body any) *httptest.ResponseRecorder {
		return doRequestWithAuth(t, env.rt, http.MethodPost, "/api/orgs/"+env.orgID+"/rules", env.admin, body)
	}
	const broken = `{"id":"old-sev","severity":"critical","rawTypeMatch":"^SET$"}`
	seedOrgRule(t, env.backend, env.orgID, "row-sev", "old-sev", broken)
	seedOrgRule(t, env.backend, env.orgID, "row-builtin", "dead-code", `{"id":"dead-code","name":"Ours","severity":"warning","rawTypeMatch":"^SET$","futureField":"kept"}`)

	rr := post(map[string]any{"id": "row-sev", "enabled": false})
	checkStatus(t, rr, http.StatusOK)
	var toggled listedOrgRule
	decodeJSON(t, rr, &toggled)
	if toggled.Enabled || toggled.ProblemReason != "INVALID_SEVERITY" {
		t.Errorf("toggle answered %+v", toggled)
	}
	stored, _ := env.backend.ListOrgCustomRules(context.Background(), env.orgID, false)
	for _, r := range stored {
		if r.ID == "row-sev" && (r.Enabled || string(r.Config) != broken) {
			t.Errorf("after the toggle: enabled %v config %s", r.Enabled, r.Config)
		}
	}
	checkStatus(t, post(map[string]any{"id": "row-sev"}), http.StatusBadRequest)
	checkStatus(t, post(map[string]any{"id": "no-such-row", "enabled": true}), http.StatusNotFound)

	// Edit the legacy rule stored under a built-in id: the echo goes back as the editor sends it.
	rows := listOrgRulesByID(t, env)
	echo := rows["dead-code"].Config
	echo["severity"] = "error"
	rr = post(map[string]any{"id": "row-builtin", "config": echo, "enabled": true})
	checkStatus(t, rr, http.StatusOK)
	stored, _ = env.backend.ListOrgCustomRules(context.Background(), env.orgID, false)
	for _, r := range stored {
		if r.ID == "row-builtin" && !strings.Contains(string(r.Config), `"futureField":"kept"`) {
			t.Errorf("the save dropped the stored unknown field: %s", r.Config)
		}
	}

	echo["id"] = "renamed"
	rr = post(map[string]any{"id": "row-builtin", "config": echo})
	checkStatus(t, rr, http.StatusConflict)
	if e := errorBody(t, rr); e.Code != "CONFLICT" {
		t.Errorf("rename: %+v", e)
	}
	if _, ok := listOrgRulesByID(t, env)["renamed"]; ok {
		t.Error("the refused rename was stored")
	}
}

// The export emits every enabled rule in its write encoding; a rule the server skips is still emitted, with
// "problem", so CI does not silently lose it. Only a rule whose stored JSON does not decode refuses the export.
func TestOrgRules_ExportKeepsRulesThatDoNotCompile(t *testing.T) {
	env := newChannelEnv(t)
	checkStatus(t, doRequestWithAuth(t, env.rt, http.MethodPost, "/api/orgs/"+env.orgID+"/rules", env.admin,
		ruleBody("clean", "^SET$", "error")), http.StatusOK)
	seedOrgRule(t, env.backend, env.orgID, "row-sev", "old-sev", `{"id":"old-sev","severity":"critical","rawTypeMatch":"^SET$"}`)
	seedOrgRule(t, env.backend, env.orgID, "row-future", "future", `{"kind":"flow","id":"future","when":{"lacks":"Retry"}}`)

	export := func() *httptest.ResponseRecorder {
		return doRequestWithAuth(t, env.rt, http.MethodGet, "/api/orgs/"+env.orgID+"/rules/export", env.admin, nil)
	}
	rr := export()
	checkStatus(t, rr, http.StatusOK)
	var doc struct {
		CustomRulesInline []map[string]any `json:"customRulesInline"`
	}
	decodeJSON(t, rr, &doc)
	byID := map[string]map[string]any{}
	for _, r := range doc.CustomRulesInline {
		byID[r["id"].(string)] = r
	}
	if c := byID["clean"]; c == nil || c["kind"] != "block" || c["schemaVersion"] != 1.0 || c["problem"] != nil {
		t.Errorf("clean rule exported as %v", c)
	}
	if c := byID["old-sev"]; c == nil || !strings.Contains(c["problem"].(string), "invalid severity") {
		t.Errorf("a rule that no longer compiles was exported as %v, want it with its problem", c)
	}
	if c := byID["future"]; c == nil || c["kind"] != "flow" || c["when"] == nil || c["problem"] == nil {
		t.Errorf("a newer release's rule was exported as %v, want it whole, with its problem", c)
	}

	seedOrgRule(t, env.backend, env.orgID, "row-garbled", "garbled", `{"id":"garbled","severity":3}`)
	rr = export()
	checkStatus(t, rr, http.StatusConflict)
	if e := errorBody(t, rr); e.Details["ruleId"] != "garbled" || e.Details["reason"] != "INVALID_JSON" {
		t.Errorf("unreadable rule: %+v", e)
	}
}

// A newer release's rule whose fields do not decode here (ADR 0015 lets a later schemaVersion give a field another
// JSON type) is still a newer release's rule: listed as such, never overwritten, and exported as stored with its
// problem instead of failing the export.
func TestOrgRules_NewerRuleWithRetypedField(t *testing.T) {
	env := newChannelEnv(t)
	const v2 = `{"kind":"block","schemaVersion":2,"id":"house","severity":"error","nameMatch":["^Set","^Get"],"anchor":"start"}`
	seedOrgRule(t, env.backend, env.orgID, "row-v2", "house", v2)

	listed := listOrgRulesByID(t, env)["house"]
	if listed.ProblemReason != "UNSUPPORTED_VERSION" || listed.Config["schemaVersion"] != 2.0 || listed.Config["id"] != "house" {
		t.Errorf("listed as %+v, want a newer release's rule (UNSUPPORTED_VERSION, schemaVersion 2)", listed)
	}

	rr := doRequestWithAuth(t, env.rt, http.MethodPost, "/api/orgs/"+env.orgID+"/rules", env.admin,
		map[string]any{"id": "row-v2", "config": listed.Config})
	checkStatus(t, rr, http.StatusConflict)
	if e := errorBody(t, rr); e.Details["reason"] != "UNSUPPORTED_VERSION" {
		t.Errorf("save over the newer rule: %+v", e)
	}
	stored, _ := env.backend.ListOrgCustomRules(context.Background(), env.orgID, false)
	if len(stored) != 1 {
		t.Fatalf("stored after the refused save: %d rows", len(stored))
	}
	if string(stored[0].Config) != v2 {
		t.Errorf("stored after the refused save: %s", stored[0].Config)
	}

	rr = doRequestWithAuth(t, env.rt, http.MethodGet, "/api/orgs/"+env.orgID+"/rules/export", env.admin, nil)
	checkStatus(t, rr, http.StatusOK)
	var doc struct {
		CustomRulesInline []map[string]any `json:"customRulesInline"`
	}
	decodeJSON(t, rr, &doc)
	if len(doc.CustomRulesInline) != 1 {
		t.Fatalf("exported %v", doc.CustomRulesInline)
	}
	c := doc.CustomRulesInline[0]
	problem, _ := c["problem"].(string)
	nameMatch, _ := c["nameMatch"].([]any)
	if c["schemaVersion"] != 2.0 || c["anchor"] != "start" || len(nameMatch) != 2 || !strings.Contains(problem, "newer") {
		t.Errorf("exported as %v, want the stored rule whole, with its problem", c)
	}
}

// validate checks each entry strictly and on its own: a wrong JSON type or an unknown field makes that entry
// invalid with its field and reason, a rule without a matcher is valid with a warning, and the request is a 200.
func TestHandleValidateCustomRules_PerEntryProblems(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodPost, "/api/analysis/custom-rules/validate", map[string]any{"rules": []any{
		map[string]any{"id": "typo", "rawTypeMatch": "^SET$", "nameMatches": "x"},
		map[string]any{"id": "typed", "severity": 3},
		map[string]any{"id": "everything", "name": "no matcher"},
		map[string]any{"id": "future", "kind": "flow"},
	}})
	checkStatus(t, rr, http.StatusOK)
	var res struct {
		Valid, Invalid int
		Entries        []struct {
			ID, Error, Field, Reason string
			Valid                    bool
			Problems                 []struct {
				Field, Reason, Message string
				Warning                bool
			}
		}
	}
	decodeJSON(t, rr, &res)
	if res.Valid != 1 || res.Invalid != 3 || len(res.Entries) != 4 {
		t.Fatalf("valid %d invalid %d entries %d, want 1/3/4", res.Valid, res.Invalid, len(res.Entries))
	}
	for i, want := range []struct{ id, field, reason string }{
		{"typo", "nameMatches", "UNKNOWN_FIELD"},
		{"typed", "severity", "INVALID_JSON"},
		{"everything", "", ""},
		{"future", "kind", "UNKNOWN_KIND"},
	} {
		e := res.Entries[i]
		if e.ID != want.id || e.Field != want.field || e.Reason != want.reason || e.Valid != (want.reason == "") {
			t.Errorf("entry %d = %+v, want %+v", i, e, want)
		}
	}
	if p := res.Entries[2].Problems; len(p) != 1 || !p[0].Warning || p[0].Reason != "NO_MATCHER" {
		t.Errorf("no-matcher rule's problems = %+v, want one NO_MATCHER warning", p)
	}
}

// The rule tester is strict too: a rule it refuses is a 400 naming "rule.<field>", and so is a request field it
// does not know.
func TestTestCustomRule_IsStrict(t *testing.T) {
	backend := &testutil.FakeBackend{}
	rt := newTestRouter(backend, true)
	seedUserWithRole(t, rt, "dev", "dev@acme.io", auth.RoleMember)
	dev := jwtBearer(t, rt, "dev", "dev@acme.io")
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/library", dev, map[string]any{
		"name": "Main",
		"content": map[string]any{"name": "Main", "subflows": []map[string]any{{
			"name": "Main", "blocks": []map[string]any{{"rawType": "SET", "name": "Set variable", "type": "action"}}}}},
	})
	checkStatus(t, rr, http.StatusCreated)
	var created struct {
		ID string `json:"id"`
	}
	decodeJSON(t, rr, &created)

	post := func(body any) *httptest.ResponseRecorder {
		return doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/custom-rules/test", dev, body)
	}
	wantFieldError(t, post(map[string]any{"flowId": created.ID, "rule": map[string]any{"id": "t", "rawTypeMatch": "^SET$", "nameMatches": "x"}}),
		"rule.nameMatches", "UNKNOWN_FIELD")
	wantFieldError(t, post(map[string]any{"flowId": created.ID, "rule": map[string]any{"rawTypeMatch": "^SET$"}}), "rule.id", "MISSING_ID")
	checkStatus(t, post(map[string]any{"flowId": created.ID, "rule": map[string]any{"id": "t"}, "extra": 1}), http.StatusBadRequest)
	checkStatus(t, post(map[string]any{"flowId": created.ID, "rule": map[string]any{"id": "t", "rawTypeMatch": "^SET$"}}), http.StatusOK)
}
