package models

import "time"

// PolicyRule configures one analyzer rule within a policy: whether it applies and,
// optionally, the severity it should be treated as (overriding the rule default).
type PolicyRule struct {
	RuleID   string   `json:"ruleId"`
	Enabled  bool     `json:"enabled"`
	Severity Severity `json:"severity,omitempty"` // empty ⇒ use the finding's own severity
	// MaxCount tolerates up to N occurrences of this rule: findings beyond the
	// first N violate even below the gate, and the first N never violate from
	// the severity budget alone. nil ⇒ no per-rule budget. This is how a real
	// compliance document reads ("≤3 TODO comments allowed").
	MaxCount *int `json:"maxCount,omitempty"`
}

// PolicyWaiver excludes one specific finding from a policy's evaluation —
// the documented-exception mechanics a severity gate can't express. Keyed by
// the finding's stable key (Fingerprint, falling back to the legacy
// rule:block key). Optional expiry; an expired waiver is ignored.
type PolicyWaiver struct {
	FindingKey string     `json:"findingKey"`
	Reason     string     `json:"reason,omitempty"`
	ExpiresAt  *time.Time `json:"expiresAt,omitempty"`
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
	GateSeverity Severity `json:"gateSeverity,omitempty"`
	// Waivers exclude individual findings from evaluation (see PolicyWaiver).
	Waivers []PolicyWaiver `json:"waivers,omitempty"`
	// MaxErrors/MaxWarnings/MaxInfo are per-severity BUDGETS for findings at
	// or above the gate: the first N findings of that severity are tolerated
	// and the rest violate. nil (default) ⇒ budget 0 ⇒ any finding of that
	// severity at/above the gate violates — exactly the previous behavior.
	// This is what lets a policy say "0 errors, ≤5 warnings" instead of a
	// single severity boolean.
	MaxErrors   *int      `json:"maxErrors,omitempty"`
	MaxWarnings *int      `json:"maxWarnings,omitempty"`
	MaxInfo     *int      `json:"maxInfo,omitempty"`
	CreatedAt   time.Time `json:"createdAt"`
	UpdatedAt   time.Time `json:"updatedAt"`
}

// BudgetFor returns the tolerated finding count at the given severity (0 when
// no cap is configured — the strict default).
func (p Policy) BudgetFor(sev Severity) int {
	switch sev {
	case SeverityError:
		if p.MaxErrors != nil {
			return *p.MaxErrors
		}
	case SeverityWarning:
		if p.MaxWarnings != nil {
			return *p.MaxWarnings
		}
	case SeverityInfo:
		if p.MaxInfo != nil {
			return *p.MaxInfo
		}
	}
	return 0
}

// HasSeverityBudget reports whether any per-severity cap is configured.
func (p Policy) HasSeverityBudget() bool {
	return p.MaxErrors != nil || p.MaxWarnings != nil || p.MaxInfo != nil
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
	// Waived counts findings excluded by unexpired waivers (visibility into
	// how much a policy's pass leans on documented exceptions).
	Waived int `json:"waived"`
}
