package analyzer

import "pad-core/models"

// maxParseErrorFindings bounds how many ParseErrors become findings: the
// conversion was 1:1 and uncapped, a parse error costs as little as five bytes
// of input ("ELSE\n"), and every finding was serialised into the response and
// retained by the report cache — 209,715 findings (894 MB allocated) from a
// 1 MB upload. A hundred bounds what the SERVER materialises, not visibility:
// the count is always reported in full and the gate still fails on the first
// Error-severity parse error.
const maxParseErrorFindings = 100

func severityForParseError(s string) models.Severity {
	switch s {
	case "warning":
		return models.SeverityWarning
	case "info":
		return models.SeverityInfo
	default:
		return models.SeverityError
	}
}

// buildParseErrorFindings converts parse errors into findings, capped at
// maxParseErrorFindings with a summary finding when it truncates; shared by the
// per-block rule and the engine's document-level fallback so the two cannot
// drift.
func buildParseErrorFindings(errs []models.ParseError, blockID, subflowID string) []models.Finding {
	if len(errs) == 0 {
		return nil
	}
	shown := errs
	truncated := 0
	if len(shown) > maxParseErrorFindings {
		truncated = len(shown) - maxParseErrorFindings
		shown = shown[:maxParseErrorFindings]
	}

	out := make([]models.Finding, 0, len(shown)+1)
	for _, pe := range shown {
		out = append(out, models.Finding{
			RuleID:      "parse-error",
			Severity:    severityForParseError(pe.Severity),
			Title:       "Parse error (line " + itoa(pe.Line) + ")",
			Description: pe.Message,
			BlockID:     blockID,
			SubflowID:   subflowID,
			Suggestion:  "Fix the syntax error so the parser can produce a complete block tree.",
			Metadata:    map[string]interface{}{"line": pe.Line, "column": pe.Column, "snippet": pe.Snippet},
		})
	}

	if truncated == 0 {
		return out
	}

	// Report the truncation instead of silently dropping it; the summary
	// carries the highest severity among the errors NOT shown, so truncation
	// can never lower the report's severity ceiling.
	worst := models.SeverityInfo
	rank := map[models.Severity]int{models.SeverityInfo: 1, models.SeverityWarning: 2, models.SeverityError: 3}
	for _, pe := range errs[maxParseErrorFindings:] {
		if s := severityForParseError(pe.Severity); rank[s] > rank[worst] {
			worst = s
		}
	}
	out = append(out, models.Finding{
		RuleID:   "parse-error",
		Severity: worst,
		Title:    "Parse errors truncated (" + itoa(truncated) + " more)",
		Description: "This flow produced " + itoa(len(errs)) + " parse errors; the first " +
			itoa(maxParseErrorFindings) + " are listed individually and " + itoa(truncated) +
			" more were omitted from the report. A file with this many parse errors is usually " +
			"not a PAD export, or was truncated in transit.",
		BlockID:    blockID,
		SubflowID:  subflowID,
		Suggestion: "Fix the errors listed above and re-run; the remainder are very likely the same cause.",
		Metadata: map[string]interface{}{
			"totalParseErrors": len(errs),
			"shown":            maxParseErrorFindings,
			"omitted":          truncated,
		},
	})
	return out
}
