package main

import (
	"testing"

	"pad-core/models"
)

// TestShouldFail_UnknownSeverityFailsClosed is the CI-gate half of the off-list
// severity defect.
//
// shouldFail ranks severities through a map and compared rank[sev] >= min. A
// severity the map does not contain yields Go's zero value, 0, which is below
// every threshold — so an unrecognised severity meant "never trips the gate",
// the single worst direction for a CI gate to guess in. Demonstrated with the
// real binary before the fix: a custom rule declaring severity "critical"
// printed [CRITICAL] ... and exited 0 under -fail-on info.
//
// Custom rules are now validated at load, so this is defence in depth: reaching
// here means a report produced by a NEWER binary than this one, and the safe
// reading of "a severity I don't know" is "at least as bad as the threshold".
func TestShouldFail_UnknownSeverityFailsClosed(t *testing.T) {
	findings := []models.Finding{{RuleID: "org-critical", Severity: models.Severity("critical")}}

	for _, threshold := range []string{"info", "warning", "error"} {
		if !shouldFail(findings, threshold) {
			t.Errorf("a finding with an unrecognised severity did not trip the gate at "+
				"-fail-on %s — the build goes green on a finding nothing can classify", threshold)
		}
	}
}
