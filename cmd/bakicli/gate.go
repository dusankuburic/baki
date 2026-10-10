package main

import (
	"fmt"

	"pad-core/analyzer"
	"pad-core/models"
)

// isValidFailOn reports whether s is a gate threshold; validated everywhere
// because shouldFail silently falls back to error-gating on a typo. "none" is
// rejected — the GitHub Action maps it to info itself.
func isValidFailOn(s string) bool {
	switch s {
	case string(models.SeverityError), string(models.SeverityWarning), string(models.SeverityInfo):
		return true
	default:
		return false
	}
}

const failOnUsage = "must be error, warning, or info"

var severityRank = map[string]int{
	string(models.SeverityInfo):    1,
	string(models.SeverityWarning): 2,
	string(models.SeverityError):   3,
}

// atOrAbove reports whether f trips a gate at rank min. Preview findings never
// gate (analyzer.Gates); an unrecognised severity does, whatever the threshold
// (fail CLOSED: a plain lookup would rank it below every threshold).
func atOrAbove(f *models.Finding, min int) bool {
	if !analyzer.Gates(f) {
		return false
	}
	r, known := severityRank[string(f.Severity)]
	return !known || r >= min
}

func thresholdRank(threshold string) int {
	if min := severityRank[threshold]; min != 0 {
		return min
	}
	return severityRank[string(models.SeverityError)]
}

func shouldFail(findings []models.Finding, threshold string) bool {
	min := thresholdRank(threshold)
	for i := range findings {
		if atOrAbove(&findings[i], min) {
			return true
		}
	}
	return false
}

// countAtOrAbove counts the findings shouldFail would trip on, so "N at or
// above X" is non-zero exactly when the gate fails.
func countAtOrAbove(findings []models.Finding, threshold string) int {
	min := thresholdRank(threshold)
	n := 0
	for i := range findings {
		if atOrAbove(&findings[i], min) {
			n++
		}
	}
	return n
}

// previewNote is the text output's " (N preview findings not gated)", or ""
// when there is none.
func previewNote(findings []models.Finding) string {
	_, n := analyzer.GatingFindings(findings)
	switch n {
	case 0:
		return ""
	case 1:
		return " (1 preview finding not gated)"
	default:
		return fmt.Sprintf(" (%d preview findings not gated)", n)
	}
}
