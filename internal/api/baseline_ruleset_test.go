package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"pad-core/analyzer"
	"pad-core/models"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// TestBaseline_RecordsTheRuleSetAndSplitsDrift: setting a baseline records the analysis' rule set (ADR 0016), and
// the drift endpoint says which new findings come from rules newer than the baseline's set: none against the set it
// was taken under, the missing rule's against an older set, and the revised rules' against a legacy baseline. New
// and its counts keep every new finding. The baseline responses carry the set's digest, never the set: it names
// every enabled org custom rule, and a flow's readers need not be the org's members.
func TestBaseline_RecordsTheRuleSetAndSplitsDrift(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	seedFlowWithFinding(t, rt, "flow1", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")
	ctx := context.Background()

	digestOnly := func(t *testing.T, what string, rec *httptest.ResponseRecorder) string {
		t.Helper()
		checkStatus(t, rec, http.StatusOK)
		var body map[string]json.RawMessage
		decodeJSON(t, rec, &body)
		if _, leaked := body["ruleSet"]; leaked {
			t.Fatalf("%s response carries the rule set: %s", what, body["ruleSet"])
		}
		var digest string
		if err := json.Unmarshal(body["ruleSetDigest"], &digest); err != nil || !strings.HasPrefix(digest, "rs1:") {
			t.Fatalf("%s response ruleSetDigest = %s (%v); want the analysis' digest", what, body["ruleSetDigest"], err)
		}
		return digest
	}
	setDigest := digestOnly(t, "set", doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/baseline/set", bearer, map[string]any{"flowId": "flow1"}))
	getDigest := digestOnly(t, "get", doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/baseline/get", bearer, map[string]any{"flowId": "flow1"}))
	stored, err := rt.security.Backend.GetFlowBaseline(ctx, "flow1")
	if err != nil || stored == nil || stored.RuleSet == nil || len(stored.RuleSet.Rules) == 0 {
		t.Fatalf("stored baseline = %+v, %v; want the analysis' rule set", stored, err)
	}
	if getDigest != setDigest || stored.RuleSet.Digest != setDigest {
		t.Fatalf("digests: set %s, get %s, stored %s; want one", setDigest, getDigest, stored.RuleSet.Digest)
	}
	bl := stored

	type driftBody struct {
		HasBaseline     bool             `json:"hasBaseline"`
		New             []models.Finding `json:"new"`
		NewRuleIDs      []string         `json:"newRuleIds"`
		NewFromNewRules int              `json:"newFromNewRules"`
	}
	drift := func(t *testing.T) driftBody {
		t.Helper()
		rec := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/baseline/drift", bearer, map[string]any{"flowId": "flow1"})
		checkStatus(t, rec, http.StatusOK)
		var d driftBody
		decodeJSON(t, rec, &d)
		return d
	}
	// Under its own rule set with no keys accepted: everything is new, none of it from a new rule.
	if err := rt.security.Backend.SetFlowBaseline(ctx, &storageif.FlowBaseline{FlowID: "flow1", Keys: []string{}, RuleSet: bl.RuleSet}); err != nil {
		t.Fatal(err)
	}
	d := drift(t)
	if !d.HasBaseline || len(d.New) == 0 || d.NewFromNewRules != 0 || len(d.NewRuleIDs) != 0 {
		t.Fatalf("drift against the same rule set = %+v; want new findings, none from new rules", d)
	}
	fired := map[string]bool{}
	for _, f := range d.New {
		fired[f.RuleID] = true
	}
	if !fired["hardcoded-url"] {
		t.Fatalf("precondition: the flow's findings %v do not include hardcoded-url", fired)
	}

	// Against a set without hardcoded-url: its findings are the new rule's.
	var refs []models.RuleRef
	for _, r := range bl.RuleSet.Rules {
		if r.ID != "hardcoded-url" {
			refs = append(refs, r)
		}
	}
	olderSet := analyzer.NewRuleSet(refs)
	if err := rt.security.Backend.SetFlowBaseline(ctx, &storageif.FlowBaseline{FlowID: "flow1", Keys: []string{}, RuleSet: olderSet}); err != nil {
		t.Fatal(err)
	}
	d = drift(t)
	want := 0
	for _, f := range d.New {
		if f.RuleID == "hardcoded-url" {
			want++
		}
	}
	if !slices.Equal(d.NewRuleIDs, []string{"hardcoded-url"}) || d.NewFromNewRules != want {
		t.Fatalf("drift against an older set = ids %v, %d from new rules; want [hardcoded-url], %d", d.NewRuleIDs, d.NewFromNewRules, want)
	}

	// A legacy baseline (no rule set): hardcoded-url is at revision 2, so it is new to it as well.
	if err := rt.security.Backend.SetFlowBaseline(ctx, &storageif.FlowBaseline{FlowID: "flow1", Keys: []string{}}); err != nil {
		t.Fatal(err)
	}
	d = drift(t)
	if !slices.Contains(d.NewRuleIDs, "hardcoded-url") {
		t.Fatalf("drift against a legacy baseline = ids %v; want hardcoded-url among them", d.NewRuleIDs)
	}
}
