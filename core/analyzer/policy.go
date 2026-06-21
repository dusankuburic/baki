package analyzer

import "pad-core/models"

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

// EvaluatePolicy gates an analysis report against a policy: it considers only the
// findings from the policy's enabled rules (applying any per-rule severity
// override), counts them, and flags as violations those whose effective severity
// meets or exceeds the policy's GateSeverity. A policy with no (or an unknown)
// GateSeverity is report-only and always passes.
//
// It evaluates an already-computed report rather than re-running analysis, so a
// flow can be analyzed once and gated against many policies. It is a pure
// function — no I/O — and safe on a nil report.
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
	gate := policySeverityRank(policy.GateSeverity)

	for _, f := range report.Findings {
		override, ok := enabled[f.RuleID]
		if !ok {
			continue // this rule is not part of the policy
		}
		eff := f.Severity
		if override != "" {
			eff = override
		}

		res.Evaluated++
		switch eff {
		case models.SeverityError:
			res.Errors++
		case models.SeverityWarning:
			res.Warnings++
		case models.SeverityInfo:
			res.Info++
		}

		if gate > 0 && policySeverityRank(eff) >= gate {
			v := f
			v.Severity = eff // surface the effective (possibly overridden) severity
			res.Violations = append(res.Violations, v)
		}
	}

	res.Passed = len(res.Violations) == 0
	return res
}
