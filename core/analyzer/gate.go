package analyzer

import (
	"strings"

	"pad-core/models"
)

// Gates reports whether f may fail a gate: the bakicli exit code, the CI
// webhook verdict, a policy, a drift or regression alert, the health score.
// Only a finding known to come from a preview rule does not: one stamped
// Stage "preview", or an unstamped one whose rule is a preview built-in
// (JSON from before stages, test fixtures). A finding stamped "stable" (a
// custom rule under a built-in's ID) gates whatever its ID. Anything else
// gates, an unknown stage or an unregistered rule included (fail closed).
func Gates(f *models.Finding) bool {
	return gates(metaTable(), f)
}

func gates(t ruleMetaTable, f *models.Finding) bool {
	switch f.Stage {
	case string(StagePreview):
		return false
	case string(StageStable):
		return true
	}
	m := t[f.RuleID]
	return m == nil || m.Stage != StagePreview
}

// GatingFindings returns the findings that may gate and how many preview
// findings it set aside. With nothing set aside it returns fs itself.
func GatingFindings(fs []models.Finding) (gating []models.Finding, preview int) {
	t := metaTable()
	for i := range fs {
		if !gates(t, &fs[i]) {
			preview++
		}
	}
	if preview == 0 {
		return fs, 0
	}
	gating = make([]models.Finding, 0, len(fs)-preview)
	for i := range fs {
		if gates(t, &fs[i]) {
			gating = append(gating, fs[i])
		}
	}
	return gating, preview
}

// gateSeverity is s as the gates rank it: an unknown severity (a stored
// report from before the engine normalised configured severities, a custom
// "critical") counts as an error, as in bakicli's gate. Never fail open on it:
// a "critical" once vanished from the counts and turned CI green.
func gateSeverity(s models.Severity) models.Severity {
	if s.Valid() {
		return s
	}
	if n := NormalizeSeverity(string(s)); n != "" {
		return n
	}
	return models.SeverityError
}

// NormalizeSeverity reads a configured severity the way the engine applies it: case and spacing ignored, "" for
// blank, anything else that is not error, warning or info as error.
func NormalizeSeverity(s string) models.Severity {
	sev := models.Severity(strings.ToLower(strings.TrimSpace(s)))
	if sev == "" || sev.Valid() {
		return sev
	}
	return models.SeverityError
}

// GateCounts tallies the gating findings by severity (an unknown severity
// counts as an error, unlike AnalysisStats) and the preview findings set aside.
type GateCounts struct {
	Errors, Warnings, Info int
	Preview                int
}

// CountGating counts fs for a severity gate: the CI webhook's verdict, the
// scanner's drift counts.
func CountGating(fs []models.Finding) GateCounts {
	t := metaTable()
	var c GateCounts
	for i := range fs {
		f := &fs[i]
		if !gates(t, f) {
			c.Preview++
			continue
		}
		switch gateSeverity(f.Severity) {
		case models.SeverityError:
			c.Errors++
		case models.SeverityWarning:
			c.Warnings++
		case models.SeverityInfo:
			c.Info++
		}
	}
	return c
}
