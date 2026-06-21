package models

import "time"

// PolicyRule configures one analyzer rule within a policy: whether it applies and,
// optionally, the severity it should be treated as (overriding the rule default).
type PolicyRule struct {
	RuleID   string   `json:"ruleId"`
	Enabled  bool     `json:"enabled"`
	Severity Severity `json:"severity,omitempty"` // empty ⇒ use the finding's own severity
}

// Policy is a named, reusable governance ruleset with a pass/fail gate. It selects
// which rules count and at what severity, and the lowest severity that fails the
// gate. Policies are evaluated against an existing analysis report (analyze once,
// gate against any number of policies), so they are independent of how analysis
// was run. Org assignment/storage layers on top of this core type.
type Policy struct {
	ID          string       `json:"id"`
	OrgID       string       `json:"orgId,omitempty"`
	Name        string       `json:"name"`
	Description string       `json:"description,omitempty"`
	Rules       []PolicyRule `json:"rules"`
	// GateSeverity is the lowest severity that fails evaluation: "error",
	// "warning", or "info". Empty (or any other value) means report-only — the
	// policy never fails, it just surfaces matching findings.
	GateSeverity Severity  `json:"gateSeverity,omitempty"`
	CreatedAt    time.Time `json:"createdAt"`
	UpdatedAt    time.Time `json:"updatedAt"`
}

// PolicyResult is the outcome of evaluating a report against a Policy.
type PolicyResult struct {
	PolicyID   string `json:"policyId"`
	PolicyName string `json:"policyName"`
	Passed     bool   `json:"passed"`
	// Violations are findings from the policy's enabled rules whose effective
	// severity meets or exceeds GateSeverity. Empty when the policy passes.
	Violations []Finding `json:"violations"`
	// Counts over the policy's enabled rules (effective severity), whether or not
	// they breach the gate.
	Errors    int `json:"errors"`
	Warnings  int `json:"warnings"`
	Info      int `json:"info"`
	Evaluated int `json:"evaluated"` // number of findings considered (from enabled rules)
}
