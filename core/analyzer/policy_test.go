package analyzer

import (
	"encoding/json"
	"strings"
	"testing"
	"time"

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

func intPtr(n int) *int { return &n }

// TestEvaluatePolicy_SeverityBudget pins R2-2's count thresholds: a
// per-severity cap tolerates the first N findings of that severity and fails
// on the rest — "0 errors, ≤5 warnings" instead of a severity boolean.
func TestEvaluatePolicy_SeverityBudget(t *testing.T) {
	// 6 warnings, 1 error. Budgets: errors 0 (default), warnings 5.
	findings := make([]models.Finding, 0, 7)
	for i := 0; i < 6; i++ {
		findings = append(findings, models.Finding{RuleID: "todo-comment", BlockID: string(rune('a' + i)), Severity: models.SeverityWarning})
	}
	findings = append(findings, models.Finding{RuleID: "hardcoded-credential", BlockID: "z", Severity: models.SeverityError})
	report := policyReport(findings...)
	policy := models.Policy{
		ID: "p", Name: "Budgets",
		Rules: []models.PolicyRule{
			{RuleID: "todo-comment", Enabled: true},
			{RuleID: "hardcoded-credential", Enabled: true},
		},
		GateSeverity: models.SeverityWarning,
		MaxWarnings:  intPtr(5),
	}

	res := EvaluatePolicy(report, policy)
	if res.Passed {
		t.Error("6 warnings over a 5 budget must fail")
	}
	if len(res.Violations) != 2 {
		t.Fatalf("want exactly the 6th warning + the error as violations, got %d", len(res.Violations))
	}
	// The tolerated five must NOT be violations.
	seen := map[string]bool{}
	for _, v := range res.Violations {
		seen[v.RuleID+"|"+v.BlockID] = true
	}
	if seen["todo-comment|f"] != true { // string(rune('a'+5)) == "f"
		t.Errorf("6th warning missing from violations: %+v", res.Violations)
	}
	if seen["todo-comment|a"] {
		t.Errorf("first (tolerated) warning wrongly violated: %+v", res.Violations)
	}

	// Within budget → passes; errors still hard-fail (budget 0).
	within := policy
	within.MaxWarnings = intPtr(6)
	if res := EvaluatePolicy(report, within); res.Passed {
		t.Error("the unbudgeted error must still fail the gate")
	}
	noError := policyReport(findings[:6]...)
	if res := EvaluatePolicy(noError, within); !res.Passed {
		t.Errorf("6 warnings within a 6 budget must pass: %+v", res)
	}
}

// TestEvaluatePolicy_PerRuleCap pins per-rule occurrence caps: the first N
// occurrences of a capped rule are tolerated even at/above the gate; every
// finding beyond violates regardless of severity.
func TestEvaluatePolicy_PerRuleCap(t *testing.T) {
	report := policyReport(
		models.Finding{RuleID: "todo-comment", BlockID: "a", Severity: models.SeverityInfo},
		models.Finding{RuleID: "todo-comment", BlockID: "b", Severity: models.SeverityInfo},
		models.Finding{RuleID: "todo-comment", BlockID: "c", Severity: models.SeverityInfo},
		models.Finding{RuleID: "todo-comment", BlockID: "d", Severity: models.SeverityInfo},
	)
	policy := models.Policy{
		ID: "p", Name: "Todos",
		Rules: []models.PolicyRule{
			{RuleID: "todo-comment", Enabled: true, MaxCount: intPtr(3)},
		},
		// Gate error-level: without the rule cap these 4 infos would all be
		// tolerated; the cap makes the 4th violate on its own.
		GateSeverity: models.SeverityError,
	}

	res := EvaluatePolicy(report, policy)
	if res.Passed {
		t.Error("4 occurrences over a 3 cap must fail")
	}
	if len(res.Violations) != 1 || res.Violations[0].BlockID != "d" {
		t.Fatalf("want exactly the 4th occurrence as violation, got %+v", res.Violations)
	}

	// Within cap → pass.
	ok := policy
	ok.Rules[0].MaxCount = intPtr(4)
	if res := EvaluatePolicy(report, ok); !res.Passed {
		t.Errorf("4 occurrences within a 4 cap must pass: %+v", res)
	}
}

// TestEvaluatePolicy_Waivers pins the documented-exception mechanics: a
// waived finding (by fingerprint key) is excluded entirely, an expired waiver
// is ignored, and the waived count surfaces for visibility.
func TestEvaluatePolicy_Waivers(t *testing.T) {
	report := policyReport(
		models.Finding{RuleID: "hardcoded-credential", BlockID: "b1", Severity: models.SeverityError, Fingerprint: "fp-1"},
		models.Finding{RuleID: "hardcoded-credential", BlockID: "b2", Severity: models.SeverityError, Fingerprint: "fp-2"},
	)
	base := models.Policy{
		ID: "p", Name: "Security",
		Rules:        []models.PolicyRule{{RuleID: "hardcoded-credential", Enabled: true}},
		GateSeverity: models.SeverityError,
	}

	// Waive one: only the other violates.
	waived := base
	waived.Waivers = []models.PolicyWaiver{{FindingKey: "fp-1", Reason: "test fixture"}}
	res := EvaluatePolicy(report, waived)
	if res.Waived != 1 {
		t.Errorf("Waived = %d, want 1", res.Waived)
	}
	if len(res.Violations) != 1 || res.Violations[0].Fingerprint != "fp-2" {
		t.Fatalf("waived finding must not violate: %+v", res.Violations)
	}

	// Waive both → passes.
	both := waived
	both.Waivers = append(both.Waivers, models.PolicyWaiver{FindingKey: "fp-2"})
	if res := EvaluatePolicy(report, both); !res.Passed || res.Waived != 2 {
		t.Errorf("fully waived policy must pass: %+v", res)
	}

	// Expired waiver is ignored (finding live again).
	expired := base
	past := timeNow().Add(-1 * timeHour)
	expired.Waivers = []models.PolicyWaiver{{FindingKey: "fp-1", ExpiresAt: &past}}
	res = EvaluatePolicy(report, expired)
	if res.Waived != 0 || len(res.Violations) != 2 {
		t.Errorf("expired waiver must be ignored: %+v", res)
	}
}

// timeNow/timeHour keep the waiver-expiry test readable without importing
// time at every use site.
var (
	timeNow  = func() time.Time { return time.Now() }
	timeHour = time.Hour
)

// TestEvaluatePolicy_PreviewNeverGates pins D3 for policies: a preview finding
// of an enabled rule is counted in Preview only, whatever the gate, the
// severity override, a waiver or a per-rule budget says.
func TestEvaluatePolicy_PreviewNeverGates(t *testing.T) {
	withPreviewRule(t, "wait-zero")
	report := policyReport(
		models.Finding{RuleID: "wait-zero", BlockID: "b1", Severity: models.SeverityError, Fingerprint: "fp-wz1"},
		models.Finding{RuleID: "wait-zero", BlockID: "b2", Severity: models.SeverityWarning, Fingerprint: "fp-wz2"},
		models.Finding{RuleID: "dead-code", BlockID: "b3", Severity: models.SeverityError, Stage: "preview", Fingerprint: "fp-dc1"},
		models.Finding{RuleID: "dead-code", BlockID: "b4", Severity: models.SeverityInfo, Fingerprint: "fp-dc2"},
		models.Finding{RuleID: "dead-code", BlockID: "b5", Severity: models.SeverityInfo, Fingerprint: "fp-dc3"},
		models.Finding{RuleID: "unhandled-error", BlockID: "b6", Severity: models.SeverityError, Stage: "preview"},
	)
	policy := models.Policy{
		ID: "p", Name: "Strict",
		Rules: []models.PolicyRule{
			{RuleID: "wait-zero", Enabled: true, Severity: models.SeverityError},
			{RuleID: "dead-code", Enabled: true, MaxCount: intPtr(1)},
		},
		GateSeverity: models.SeverityInfo,
		Waivers:      []models.PolicyWaiver{{FindingKey: "fp-wz1"}},
	}

	res := EvaluatePolicy(report, policy)
	// unhandled-error is not in the policy: neither evaluated nor counted.
	if res.Preview != 3 || res.Waived != 0 || res.Evaluated != 2 {
		t.Errorf("want Preview 3, Waived 0, Evaluated 2, got %+v", res)
	}
	if res.Errors != 0 || res.Warnings != 0 || res.Info != 2 {
		t.Errorf("preview findings must not reach the severity counts: %+v", res)
	}
	// The dead-code budget of 1 is spent by b4, not by the preview b3, so b5
	// alone violates.
	if res.Passed || len(res.Violations) != 1 || res.Violations[0].BlockID != "b5" {
		t.Fatalf("want b5 as the lone violation, got %+v", res.Violations)
	}

	onlyPreview := policy
	onlyPreview.Rules = onlyPreview.Rules[:1]
	if res := EvaluatePolicy(report, onlyPreview); !res.Passed || res.Preview != 2 || res.Evaluated != 0 {
		t.Errorf("a policy over a preview rule alone must pass: %+v", res)
	}
}

func TestPolicyResult_PreviewOmittedWhenZero(t *testing.T) {
	b, err := json.Marshal(models.PolicyResult{})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "preview") {
		t.Errorf("a zero Preview must not change the wire shape: %s", b)
	}
	b, _ = json.Marshal(models.PolicyResult{Preview: 2})
	if !strings.Contains(string(b), `"preview":2`) {
		t.Errorf("Preview missing: %s", b)
	}
}

// TestEvaluatePolicy_UnknownSeverityFailsClosed: an unknown finding or
// override severity ranks as an error, even at an info gate (it ranked below
// every gate and the policy passed).
func TestEvaluatePolicy_UnknownSeverityFailsClosed(t *testing.T) {
	report := policyReport(
		models.Finding{RuleID: "dead-code", BlockID: "b1", Severity: "critical"},
		models.Finding{RuleID: "missing-delay", BlockID: "b2", Severity: models.SeverityInfo},
	)
	policy := models.Policy{
		Rules: []models.PolicyRule{
			{RuleID: "dead-code", Enabled: true},
			{RuleID: "missing-delay", Enabled: true, Severity: "blocker"},
		},
		GateSeverity: models.SeverityError,
	}
	res := EvaluatePolicy(report, policy)
	if res.Passed || len(res.Violations) != 2 || res.Errors != 2 {
		t.Errorf("both findings must violate as errors: %+v", res)
	}
}

// s2-3: a policy's severities read like settings severities: case and spacing are ignored, so a "Warning"
// override downgrades the rule (it used to count as an unknown severity, an error) and an "Error" gate gates.
func TestEvaluatePolicy_SeveritiesIgnoreCaseAndSpacing(t *testing.T) {
	report := policyReport(models.Finding{RuleID: "dead-code", BlockID: "b1", Severity: models.SeverityError})
	downgraded := EvaluatePolicy(report, models.Policy{
		Rules:        []models.PolicyRule{{RuleID: "dead-code", Enabled: true, Severity: " Warning"}},
		GateSeverity: models.SeverityError,
	})
	if !downgraded.Passed || downgraded.Warnings != 1 || downgraded.Errors != 0 {
		t.Errorf("a \" Warning\" override must downgrade the finding: %+v", downgraded)
	}
	gated := EvaluatePolicy(report, models.Policy{
		Rules:        []models.PolicyRule{{RuleID: "dead-code", Enabled: true}},
		GateSeverity: "Error",
	})
	if gated.Passed {
		t.Errorf("an \"Error\" gate must gate, not report only: %+v", gated)
	}
}
