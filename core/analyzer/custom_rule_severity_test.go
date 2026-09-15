package analyzer

import (
	"strings"
	"testing"

	"pad-core/models"
)

// TestNewCustomRule_RejectsOffListSeverity is the regression gate for a config
// value that looked configured and enforced nothing.
//
// Severity is a plain string type so it can round-trip through JSON, and
// NewCustomRule used to do models.Severity(cfg.Severity) with no check. Every
// downstream consumer then dropped the off-list value silently, each in a
// different way: the Markdown/HTML/PDF exporters iterate a fixed severityOrder
// so the bucket is never rendered (the report showed Health Score 100/100 with
// an empty body); JUnit emits a <failure> only for error/warning, so the
// finding became a PASSING testcase and CI went green; SARIF's level mapping
// fell through to "warning"; and bakicli's shouldFail ranked it 0, below every
// threshold, so even -fail-on info exited 0.
//
// Reachable from POST /api/orgs/{id}/rules (org admin) and from
// bakicli -custom-rules. An org author writing "critical" — the obvious word —
// got exactly this.
func TestNewCustomRule_RejectsOffListSeverity(t *testing.T) {
	for _, bad := range []string{"critical", "CRITICAL", "high", "medium", "fatal", "err"} {
		t.Run(bad, func(t *testing.T) {
			_, err := NewCustomRule(CustomRuleConfig{
				ID:        "org-rule",
				Severity:  bad,
				NameMatch: "x",
			})
			if err == nil {
				t.Fatalf("severity %q was accepted; it is dropped silently by every exporter "+
					"and can never fail the CI gate", bad)
			}
			// The message must name the offending value and the valid set, or
			// the author cannot act on it.
			if !strings.Contains(err.Error(), bad) {
				t.Errorf("error does not name the bad value: %v", err)
			}
			for _, want := range []string{"error", "warning", "info"} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("error does not list the valid severity %q: %v", want, err)
				}
			}
		})
	}
}

// TestNewCustomRule_AcceptsEveryValidSeverity keeps the check from being drawn
// too wide — rejecting a legitimate value would break every existing org rule.
func TestNewCustomRule_AcceptsEveryValidSeverity(t *testing.T) {
	for _, good := range models.ValidSeverities() {
		t.Run(string(good), func(t *testing.T) {
			r, err := NewCustomRule(CustomRuleConfig{
				ID:        "org-rule",
				Severity:  string(good),
				NameMatch: "x",
			})
			if err != nil {
				t.Fatalf("valid severity %q rejected: %v", good, err)
			}
			if r.DefaultSeverity() != good {
				t.Errorf("severity = %q, want %q", r.DefaultSeverity(), good)
			}
		})
	}
}

// TestNewCustomRule_EmptySeverityStillDefaults preserves the documented default.
func TestNewCustomRule_EmptySeverityStillDefaults(t *testing.T) {
	r, err := NewCustomRule(CustomRuleConfig{ID: "org-rule", NameMatch: "x"})
	if err != nil {
		t.Fatalf("empty severity rejected: %v", err)
	}
	if r.DefaultSeverity() != models.SeverityWarning {
		t.Errorf("default severity = %q, want warning", r.DefaultSeverity())
	}
}

// TestSeverity_Valid pins the set itself, so widening it is a deliberate edit
// that forces the exporters and the gate to be revisited in the same change.
func TestSeverity_Valid(t *testing.T) {
	for _, s := range models.ValidSeverities() {
		if !s.Valid() {
			t.Errorf("%q is in ValidSeverities but Valid() says otherwise", s)
		}
	}
	for _, s := range []models.Severity{"", "critical", "Error", "none"} {
		if s.Valid() {
			t.Errorf("%q reported valid; every exporter and the CI gate handles only "+
				"error/warning/info", s)
		}
	}
}
