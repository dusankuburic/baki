package analyzer

import "pad-core/models"

// maxParseErrorFindings bounds how many ParseErrors are converted into
// findings.
//
// The conversion was 1:1 and uncapped, and a parse error costs as little as five
// bytes of input ("ELSE\n"), so the finding count scaled with the file size:
// measured 2,048 findings from 10 KB, 20,480 from 100 KB and 209,715 from 1 MB,
// the last allocating 894 MB — against an endpoint that accepts 2 MB. Each
// finding is then serialised into the response, which is where the ~141x
// request-to-response amplification and the multi-hundred-megabyte bodies came
// from, and each is retained by the report cache afterwards.
//
// A hundred is far more than anyone acts on: a file this broken is fixed by
// looking at the first few errors, not the twenty-thousandth. The cap is about
// what the SERVER will materialise, not about hiding the problem — the count is
// always reported in full, and the gate still fails because parse-error is Error
// severity and the first one is enough.
const maxParseErrorFindings = 100

// severityForParseError maps a ParseError's severity string onto the model.
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
// maxParseErrorFindings and followed by a summary finding when it truncates.
//
// It is shared by the per-block rule and the engine's document-level fallback so
// the two cannot drift — they previously carried the same loop twice.
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

	// Report the truncation instead of silently dropping it. A report that
	// quietly omits findings is indistinguishable from a cleaner file, which is
	// the failure this codebase has had to correct more than once.
	//
	// The summary carries the highest severity among the errors that were NOT
	// shown, so truncation can never lower the report's severity ceiling.
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
