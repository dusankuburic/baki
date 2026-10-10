package api

import (
	"errors"

	"pad-analyzer/internal/platform/apperr"
	"pad-core/analyzer"
)

// customRuleError maps a custom rule the strict check refuses (*analyzer.CustomRuleError anywhere in err's chain)
// to 400 BAD_REQUEST "invalid rule: …" with details.field (prefix + the rule's JSON field, e.g. "config.severity";
// absent for the rule as a whole) and details.reason (the analyzer's reason code). Any other error is returned as it
// is (docs/custom-rules.md, "Errors").
func customRuleError(err error, prefix string) error {
	var ce *analyzer.CustomRuleError
	if err == nil || !errors.As(err, &ce) {
		return err
	}
	details := map[string]any{"reason": ce.Reason}
	if ce.Field != "" {
		details["field"] = prefix + ce.Field
	}
	return apperr.Invalid("", "invalid rule: "+ce.Error()).WithDetails(details).Wrap(err)
}

// customRuleProblemOut is one problem of one validated rule: an error, or a warning the rule still runs with.
type customRuleProblemOut struct {
	Field   string `json:"field,omitempty"`
	Reason  string `json:"reason"`
	Message string `json:"message"`
	Warning bool   `json:"warning,omitempty"`
}

func toCustomRuleProblems(problems []*analyzer.CustomRuleError) []customRuleProblemOut {
	if len(problems) == 0 {
		return nil
	}
	out := make([]customRuleProblemOut, len(problems))
	for i, p := range problems {
		out[i] = customRuleProblemOut{Field: p.Field, Reason: p.Reason, Message: p.Error(), Warning: p.Warning}
	}
	return out
}
