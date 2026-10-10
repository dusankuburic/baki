package analyzer

import (
	"testing"

	"pad-core/models"
)

// previewMetaOf returns id's metadata moved to the preview stage.
func previewMetaOf(t *testing.T, id string) RuleMeta {
	t.Helper()
	m, ok := LookupMeta(id)
	if !ok {
		t.Fatalf("%s is not a built-in", id)
	}
	m.Stage = StagePreview
	return m
}

// withPreviewRule flips the built-in id to the preview stage for the test.
func withPreviewRule(t *testing.T, id string) {
	t.Helper()
	t.Cleanup(SetRuleMetaForTesting(id, previewMetaOf(t, id)))
}

func TestGates(t *testing.T) {
	withPreviewRule(t, "wait-zero")
	cases := []struct {
		name string
		f    models.Finding
		want bool
	}{
		{"stable rule", models.Finding{RuleID: "dead-code"}, true},
		{"stamped preview", models.Finding{RuleID: "dead-code", Stage: "preview"}, false},
		{"stamped preview, unregistered rule", models.Finding{RuleID: "org-rule", Stage: "preview"}, false},
		{"unstamped finding of a preview rule", models.Finding{RuleID: "wait-zero"}, false},
		{"unknown stage of a preview rule", models.Finding{RuleID: "wait-zero", Stage: "beta"}, false},
		{"unknown stage of a stable rule", models.Finding{RuleID: "dead-code", Stage: "beta"}, true},
		{"explicit stable stamp", models.Finding{RuleID: "dead-code", Stage: "stable"}, true},
		{"unregistered rule", models.Finding{RuleID: "org-rule"}, true},
		{"stage is case-sensitive", models.Finding{RuleID: "dead-code", Stage: "Preview"}, true},
	}
	for _, c := range cases {
		if got := Gates(&c.f); got != c.want {
			t.Errorf("%s: Gates = %t, want %t", c.name, got, c.want)
		}
	}
}

func TestGates_FollowsTheMetaTable(t *testing.T) {
	f := models.Finding{RuleID: "wait-zero"}
	if !Gates(&f) {
		t.Fatal("wait-zero is stable: its finding must gate")
	}
	restore := SetRuleMetaForTesting("wait-zero", previewMetaOf(t, "wait-zero"))
	if Gates(&f) {
		t.Error("after the flip to preview the unstamped finding must not gate")
	}
	restore()
	if !Gates(&f) {
		t.Error("after the restore the finding must gate again")
	}
}

func TestGatingFindings(t *testing.T) {
	withPreviewRule(t, "wait-zero")
	stable := []models.Finding{
		{RuleID: "dead-code", Severity: models.SeverityError},
		{RuleID: "org-rule", Severity: "critical"},
	}
	got, preview := GatingFindings(stable)
	if preview != 0 || len(got) != 2 || &got[0] != &stable[0] {
		t.Errorf("nothing set aside: want fs itself and 0, got %d findings (same array %t), %d", len(got), len(got) > 0 && &got[0] == &stable[0], preview)
	}

	mixed := []models.Finding{
		{ID: "F-1", RuleID: "wait-zero", Severity: models.SeverityError},
		{ID: "F-2", RuleID: "dead-code", Severity: models.SeverityWarning},
		{ID: "F-3", RuleID: "dead-code", Severity: models.SeverityError, Stage: "preview"},
		{ID: "F-4", RuleID: "org-rule", Severity: "critical"},
	}
	got, preview = GatingFindings(mixed)
	if preview != 2 || len(got) != 2 || got[0].ID != "F-2" || got[1].ID != "F-4" {
		t.Errorf("want F-2, F-4 and 2 preview, got %+v and %d", got, preview)
	}
	if mixed[0].ID != "F-1" || mixed[2].ID != "F-3" {
		t.Error("GatingFindings must not reorder its input")
	}

	if got, preview := GatingFindings(nil); got != nil || preview != 0 {
		t.Errorf("nil: got %v, %d", got, preview)
	}
}

func TestCountGating(t *testing.T) {
	withPreviewRule(t, "wait-zero")
	got := CountGating([]models.Finding{
		{RuleID: "dead-code", Severity: models.SeverityError},
		{RuleID: "dead-code", Severity: models.SeverityWarning},
		{RuleID: "dead-code", Severity: models.SeverityWarning},
		{RuleID: "dead-code", Severity: models.SeverityInfo},
		{RuleID: "org-rule", Severity: "critical"},
		{RuleID: "wait-zero", Severity: models.SeverityError},
		{RuleID: "dead-code", Severity: models.SeverityError, Stage: "preview"},
		{RuleID: "dead-code", Severity: "critical", Stage: "preview"},
	})
	// The unknown "critical" counts as an error (fail closed, as bakicli).
	want := GateCounts{Errors: 2, Warnings: 2, Info: 1, Preview: 3}
	if got != want {
		t.Errorf("CountGating = %+v, want %+v", got, want)
	}
	if got := CountGating(nil); got != (GateCounts{}) {
		t.Errorf("nil: %+v", got)
	}
}

// A custom rule under a preview built-in's ID: its findings gate, and they
// take nothing from the built-in's metadata (stage, autoFix, confidence).
func TestGates_CustomRuleShadowingAPreviewBuiltin(t *testing.T) {
	withPreviewRule(t, "dead-code")
	cr, _, err := CompileCustomRule(CustomRuleConfig{ID: "dead-code", Name: "Team rule",
		RawTypeMatch: "^WAIT$", Severity: "error"}, CustomRuleTolerant)
	if err != nil {
		t.Fatal(err)
	}
	doc := parseFlow(t, "#Region \"Main\"\nWAIT 1\n#EndRegion\n")
	report := RunAnalysis(doc, []Rule{cr}, nil, nil)
	if len(report.Findings) == 0 {
		t.Fatal("the custom rule found nothing")
	}
	for _, f := range report.Findings {
		if f.Stage != string(StageStable) || !Gates(&f) {
			t.Errorf("custom finding %s: stage %q, Gates %t; want stable and gating", f.ID, f.Stage, Gates(&f))
		}
		if f.AutoFix != "" || f.Confidence != models.ConfidenceMedium {
			t.Errorf("custom finding %s took the built-in's metadata: autoFix %q, confidence %q", f.ID, f.AutoFix, f.Confidence)
		}
	}
	if c := CountGating(report.Findings); c.Errors != len(report.Findings) || c.Preview != 0 {
		t.Errorf("CountGating = %+v, want every finding an error", c)
	}

	// The built-in itself stays preview, in the same run too.
	both := RunAnalysis(parseFlow(t, "#Region \"Main\"\nWAIT 1\nEXIT\nWAIT 2\n#EndRegion\n"),
		[]Rule{cr, &DeadCodeRule{}}, nil, nil)
	var builtin, custom int
	for _, f := range both.Findings {
		switch f.Stage {
		case string(StagePreview):
			builtin++
		case string(StageStable):
			custom++
		default:
			t.Errorf("finding %s (%s) is unstamped", f.ID, f.Title)
		}
	}
	if builtin == 0 || custom == 0 {
		t.Errorf("want findings of both rules, got %d preview and %d stable", builtin, custom)
	}
}

// TestSeverityOverride_UnknownFailsClosed: settings severities are not
// validated on every write path, so the engine reads them leniently (case,
// spacing) and an unknown value overrides as error rather than vanishing from
// every server-side gate.
func TestSeverityOverride_UnknownFailsClosed(t *testing.T) {
	r := &WaitZeroRule{} // default info
	cases := []struct {
		configured string
		want       models.Severity
	}{
		{"", ""},
		{"  ", ""},
		{"info", ""},
		{" Info ", ""},
		{"WARNING", models.SeverityWarning},
		{"error", models.SeverityError},
		{"critical", models.SeverityError},
		{"low", models.SeverityError},
	}
	for _, c := range cases {
		s := models.DefaultSettings()
		s.Analysis.Rules = map[string]models.RuleConfig{r.ID(): {Enabled: true, Severity: c.configured}}
		if got := severityOverride(r, s); got != c.want {
			t.Errorf("configured %q: override %q, want %q", c.configured, got, c.want)
		}
	}

	s := models.DefaultSettings()
	s.Analysis.Rules = map[string]models.RuleConfig{r.ID(): {Enabled: true, Severity: "critical"}}
	report := RunAnalysis(parseFlow(t, "#Region \"Main\"\nWAIT 0\n#EndRegion\n"), []Rule{r}, s, nil)
	if len(report.Findings) == 0 {
		t.Fatal("wait-zero found nothing")
	}
	if c := CountGating(report.Findings); c.Errors != len(report.Findings) {
		t.Errorf("a configured \"critical\" must gate as an error: %+v", c)
	}
}
