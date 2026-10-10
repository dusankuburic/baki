package analyzer

import (
	"encoding/json"
	"strings"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

func TestRuleOption_TypesAndFallback(t *testing.T) {
	spec := OptionSpec{Key: "k", Default: 7, Min: 1}
	mk := func(v any) *models.AppSettings {
		return &models.AppSettings{Analysis: models.AnalysisSettings{Rules: map[string]models.RuleConfig{
			"r": {Enabled: true, Options: map[string]any{"k": v}},
		}}}
	}
	cases := []struct {
		name string
		v    any
		want int
	}{
		{"float64 (JSON)", 4.0, 4},
		{"int (Go / DefaultSettings)", 4, 4},
		{"int64", int64(4), 4},
		{"json.Number", json.Number("4"), 4},
		{"string is ignored", "4", 7},
		{"zero falls back", 0.0, 7},
		{"negative falls back", -3, 7},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := ruleOption(mk(c.v), "r", spec); got != c.want {
				t.Errorf("ruleOption(%v) = %d, want %d", c.v, got, c.want)
			}
		})
	}
	if got := ruleOption(nil, "r", spec); got != 7 {
		t.Errorf("nil settings: got %d, want default", got)
	}
	if got := ruleOption(mk(4.0), "other", spec); got != 7 {
		t.Errorf("unknown rule: got %d, want default", got)
	}
}

// TestRuleOptions_IntThresholdHonoured runs a real rule with an int-typed
// threshold — the shape models.DefaultSettings() and every Go caller produce.
// The threshold rules used to accept float64 only, so an int was silently
// ignored and the rule fell back to its hardcoded constant. The constant
// happened to equal the default, which is why it never showed.
func TestRuleOptions_IntThresholdHonoured(t *testing.T) {
	var b strings.Builder
	b.WriteString("#Region \"Main\"\n")
	for i := 1; i <= 3; i++ {
		b.WriteString(strings.Repeat("    ", i) + "IF %A% = 1 THEN\n")
	}
	b.WriteString("                SET Ok TO 1\n")
	for i := 3; i >= 1; i-- {
		b.WriteString(strings.Repeat("    ", i) + "END\n")
	}
	b.WriteString("#EndRegion\n")
	src := b.String()
	doc, err := parser.ParseText(src, "Main.txt", int64(len(src)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	settings := models.DefaultSettings()
	settings.Analysis.Rules["deep-nesting"] = models.RuleConfig{Enabled: true, Severity: "info", Options: map[string]any{"maxDepth": 2}}
	report := RunAnalysis(doc, []Rule{&DeepNestingRule{}}, settings, nil)
	found := 0
	for _, f := range report.Findings {
		if f.RuleID == "deep-nesting" {
			found++
		}
	}
	if found == 0 {
		t.Fatal("maxDepth=2 (int) was ignored: a 3-deep flow produced no deep-nesting finding")
	}
}
