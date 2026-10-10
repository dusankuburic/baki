package analyzer

import (
	"strings"
	"time"

	"pad-core/models"
)

// policySeverityRank orders severities for gate comparisons (higher = worse).
// 0 is "unset/none", used to mean a report-only policy with no failing gate.
func policySeverityRank(s models.Severity) int {
	switch s {
	case models.SeverityInfo:
		return 1
	case models.SeverityWarning:
		return 2
	case models.SeverityError:
		return 3
	default:
		return 0
	}
}

// EvaluatePolicy gates a report against a policy: it counts only findings from
// the policy's enabled rules (with any per-rule severity override) and flags as
// violations those whose effective severity meets GateSeverity or exceeds a
// per-rule count budget. Preview findings (Gates false) are only counted in
// Preview. An unknown finding or override severity counts as an error. A
// policy with no (or unknown) GateSeverity is report-only and always passes. It evaluates an already-computed report
// (analyze once, gate against many policies) and is safe on a nil report.
func EvaluatePolicy(report *models.AnalysisReport, policy models.Policy) *models.PolicyResult {
	res := &models.PolicyResult{
		PolicyID:   policy.ID,
		PolicyName: policy.Name,
		Passed:     true,
		Violations: []models.Finding{},
	}
	if report == nil {
		return res
	}

	// Enabled rule ID → severity override ("" means keep the finding's severity).
	enabled := make(map[string]models.Severity, len(policy.Rules))
	for _, pr := range policy.Rules {
		if pr.Enabled {
			enabled[pr.RuleID] = pr.Severity
		}
	}
	// Rule-level budgets: ruleID → tolerated count (findings beyond violate).
	ruleBudgets := make(map[string]int, len(policy.Rules))
	for _, pr := range policy.Rules {
		if pr.Enabled && pr.MaxCount != nil {
			ruleBudgets[pr.RuleID] = *pr.MaxCount
		}
	}
	// Waivers: unexpired finding keys → excluded from evaluation entirely.
	waived := make(map[string]bool, len(policy.Waivers))
	now := time.Now()
	for _, w := range policy.Waivers {
		if w.ExpiresAt != nil && now.After(*w.ExpiresAt) {
			continue // expired waiver = finding is live again
		}
		waived[w.FindingKey] = true
	}
	gate := policySeverityRank(models.Severity(strings.ToLower(strings.TrimSpace(string(policy.GateSeverity)))))

	// Ordinal counters for budget accounting (stable report order): per-rule
	// occurrences, and per-effective-severity occurrences among gate-meeting
	// findings.
	ruleOrdinal := make(map[string]int)
	sevOrdinal := make(map[models.Severity]int)
	metas := metaTable()

	for i := range report.Findings {
		f := &report.Findings[i]
		override, ok := enabled[f.RuleID]
		if !ok {
			continue // this rule is not part of the policy
		}
		if !gates(metas, f) {
			res.Preview++ // preview never gates, whatever the policy says
			continue
		}
		if waived[findingWaiverKey(f)] {
			res.Waived++
			continue
		}
		eff := f.Severity
		if o := NormalizeSeverity(string(override)); o != "" {
			eff = o
		}
		eff = gateSeverity(eff)

		res.Evaluated++
		switch eff {
		case models.SeverityError:
			res.Errors++
		case models.SeverityWarning:
			res.Warnings++
		case models.SeverityInfo:
			res.Info++
		}

		violates := false
		if budget, capped := ruleBudgets[f.RuleID]; capped {
			// Per-rule budget: the first N occurrences are tolerated; every
			// finding beyond violates (a rule cap IS its own gate).
			ord := ruleOrdinal[f.RuleID]
			ruleOrdinal[f.RuleID] = ord + 1
			violates = ord >= budget
		} else if gate > 0 && policySeverityRank(eff) >= gate {
			// Severity budget: the first budget(severity) findings of this
			// severity are tolerated; findings beyond violate. Default
			// budget 0 = the previous any-finding-violates behavior.
			sev := eff
			ord := sevOrdinal[sev]
			sevOrdinal[sev] = ord + 1
			violates = ord >= policy.BudgetFor(sev)
		}

		if violates {
			v := *f
			v.Severity = eff // surface the effective (possibly overridden) severity
			res.Violations = append(res.Violations, v)
		}
	}

	res.Passed = len(res.Violations) == 0
	return res
}

// findingWaiverKey is the stable identity waivers reference — the same key
// baselines use (content Fingerprint first, legacy rule:block fallback).
func findingWaiverKey(f *models.Finding) string {
	if f.Fingerprint != "" {
		return f.Fingerprint
	}
	return f.Key()
}
