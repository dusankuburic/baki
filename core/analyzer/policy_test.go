package analyzer

import (
	"testing"

	"pad-core/models"
)

func policyReport(findings ...models.Finding) *models.AnalysisReport {
	return &models.AnalysisReport{FlowID: "f1", Findings: findings}
}

func TestEvaluatePolicy_GateFailsOnViolation(t *testing.T) {
	report := policyReport(
		models.Finding{RuleID: "hardcoded-credential", BlockID: "b1", Severity: models.SeverityError},
		models.Finding{RuleID: "dead-code", BlockID: "b2", Severity: models.SeverityInfo},
	)
	policy := models.Policy{
		ID: "p1", Name: "Security",
		Rules:        []models.PolicyRule{{RuleID: "hardcoded-credential", Enabled: true}},
		GateSeverity: models.SeverityWarning,
	}

	res := EvaluatePolicy(report, policy)
	if res.Passed {
		t.Error("expected policy to fail on an error-severity violation")
	}
	if len(res.Violations) != 1 || res.Violations[0].RuleID != "hardcoded-credential" {
		t.Fatalf("expected the credential finding as the lone violation, got %+v", res.Violations)
	}
	// dead-code is not in the policy, so it must not be counted.
	if res.Evaluated != 1 || res.Errors != 1 || res.Info != 0 {
		t.Errorf("unexpected counts: %+v", res)
	}
}

func TestEvaluatePolicy_ReportOnlyAlwaysPasses(t *testing.T) {
	report := policyReport(models.Finding{RuleID: "r1", BlockID: "b1", Severity: models.SeverityError})
	policy := models.Policy{
		Rules: []models.PolicyRule{{RuleID: "r1", Enabled: true}},
		// no GateSeverity ⇒ report-only
	}

	res := EvaluatePolicy(report, policy)
	if !res.Passed {
		t.Error("a report-only policy (no gate) must always pass")
	}
	if res.Evaluated != 1 || res.Errors != 1 {
		t.Errorf("report-only should still count findings: %+v", res)
	}
	if len(res.Violations) != 0 {
		t.Errorf("report-only should have no violations, got %d", len(res.Violations))
	}
}

func TestEvaluatePolicy_SeverityOverrideTripsGate(t *testing.T) {
	// The finding is only info, but the policy elevates the rule to error and
	// gates at error — so it should now violate.
	report := policyReport(models.Finding{RuleID: "missing-delay", BlockID: "b1", Severity: models.SeverityInfo})
	policy := models.Policy{
		Rules:        []models.PolicyRule{{RuleID: "missing-delay", Enabled: true, Severity: models.SeverityError}},
		GateSeverity: models.SeverityError,
	}

	res := EvaluatePolicy(report, policy)
	if res.Passed {
		t.Error("severity override to error should trip the error gate")
	}
	if len(res.Violations) != 1 || res.Violations[0].Severity != models.SeverityError {
		t.Fatalf("violation should carry the effective (overridden) severity, got %+v", res.Violations)
	}
	if res.Errors != 1 || res.Info != 0 {
		t.Errorf("override should be counted as an error, not info: %+v", res)
	}
}

func TestEvaluatePolicy_IgnoresDisabledAndUnlistedRules(t *testing.T) {
	report := policyReport(
		models.Finding{RuleID: "r-on", BlockID: "b1", Severity: models.SeverityError},
		models.Finding{RuleID: "r-off", BlockID: "b2", Severity: models.SeverityError},
		models.Finding{RuleID: "r-absent", BlockID: "b3", Severity: models.SeverityError},
	)
	policy := models.Policy{
		Rules: []models.PolicyRule{
			{RuleID: "r-on", Enabled: true},
			{RuleID: "r-off", Enabled: false},
		},
		GateSeverity: models.SeverityError,
	}

	res := EvaluatePolicy(report, policy)
	if res.Evaluated != 1 {
		t.Errorf("only the one enabled+listed rule should be evaluated, got %d", res.Evaluated)
	}
	if len(res.Violations) != 1 || res.Violations[0].RuleID != "r-on" {
		t.Errorf("only r-on should violate, got %+v", res.Violations)
	}
}

func TestEvaluatePolicy_NilReport(t *testing.T) {
	res := EvaluatePolicy(nil, models.Policy{GateSeverity: models.SeverityError})
	if !res.Passed || len(res.Violations) != 0 {
		t.Errorf("nil report should pass with no violations, got %+v", res)
	}
	if res.Violations == nil {
		t.Error("Violations must be a non-nil empty slice")
	}
}
