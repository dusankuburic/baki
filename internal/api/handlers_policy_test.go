package api

import (
	"net/http"
	"testing"
)

func TestPolicyEvaluate_FailsOnViolation(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	// seedFlowWithFinding (from the triage tests) plants a block with a hardcoded
	// URL, which trips the "hardcoded-url" rule.
	seedFlowWithFinding(t, rt, "flow1", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/policy/evaluate", bearer, map[string]any{
		"flowId": "flow1",
		"policy": map[string]any{
			"name":         "URL policy",
			"gateSeverity": "warning",
			"rules":        []map[string]any{{"ruleId": "hardcoded-url", "enabled": true, "severity": "error"}},
		},
	})
	checkStatus(t, rr, http.StatusOK)

	var res struct {
		Passed     bool `json:"passed"`
		Violations []struct {
			RuleID string `json:"ruleId"`
		} `json:"violations"`
	}
	decodeJSON(t, rr, &res)
	if res.Passed {
		t.Error("expected policy to fail (the flow has a hardcoded URL)")
	}
	if len(res.Violations) == 0 || res.Violations[0].RuleID != "hardcoded-url" {
		t.Errorf("expected a hardcoded-url violation, got %+v", res.Violations)
	}
}

func TestPolicyEvaluate_PassesWhenRuleNotTriggered(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	seedFlowWithFinding(t, rt, "flow1", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	// Gate only a rule the flow doesn't trigger ⇒ no violations ⇒ pass.
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/policy/evaluate", bearer, map[string]any{
		"flowId": "flow1",
		"policy": map[string]any{
			"name":         "Unrelated",
			"gateSeverity": "error",
			"rules":        []map[string]any{{"ruleId": "no-such-rule", "enabled": true}},
		},
	})
	checkStatus(t, rr, http.StatusOK)

	var res struct {
		Passed bool `json:"passed"`
	}
	decodeJSON(t, rr, &res)
	if !res.Passed {
		t.Error("expected pass when the gated rule isn't triggered")
	}
}

func TestPolicyEvaluate_NonOwnerForbidden(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	seedFlowWithFinding(t, rt, "flow1", "alice")
	bearer := jwtBearer(t, rt, "bob", "bob@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/analysis/policy/evaluate", bearer, map[string]any{
		"flowId": "flow1",
		"policy": map[string]any{"name": "x", "rules": []map[string]any{{"ruleId": "hardcoded-url", "enabled": true}}},
	})
	checkStatus(t, rr, http.StatusForbidden)
}
