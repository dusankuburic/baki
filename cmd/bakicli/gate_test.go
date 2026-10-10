package main

import (
	"testing"

	"pad-core/analyzer"
	"pad-core/models"
)

func findingsAt(severities ...models.Severity) []models.Finding {
	out := make([]models.Finding, 0, len(severities))
	for _, s := range severities {
		out = append(out, models.Finding{Severity: s})
	}
	return out
}

// TestShouldFail_Thresholds covers the decision that makes CI red or green.
// It is a ">=" over a severity rank, so each threshold must trip on its own
// level and every level above it, and on nothing below.
func TestShouldFail_Thresholds(t *testing.T) {
	cases := []struct {
		name      string
		threshold string
		findings  []models.Finding
		want      bool
	}{
		{"error threshold, error finding", "error", findingsAt(models.SeverityError), true},
		{"error threshold, warning finding", "error", findingsAt(models.SeverityWarning), false},
		{"error threshold, info finding", "error", findingsAt(models.SeverityInfo), false},
		{"warning threshold, error finding", "warning", findingsAt(models.SeverityError), true},
		{"warning threshold, warning finding", "warning", findingsAt(models.SeverityWarning), true},
		{"warning threshold, info finding", "warning", findingsAt(models.SeverityInfo), false},
		{"info threshold, info finding", "info", findingsAt(models.SeverityInfo), true},
		{"info threshold, error finding", "info", findingsAt(models.SeverityError), true},
		{"no findings never fails", "info", nil, false},
		{"mixed: one error is enough", "error",
			findingsAt(models.SeverityInfo, models.SeverityWarning, models.SeverityError), true},
		{"mixed: none reach the threshold", "error",
			findingsAt(models.SeverityInfo, models.SeverityWarning), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := shouldFail(tc.findings, tc.threshold); got != tc.want {
				t.Errorf("shouldFail(%v, %q) = %v, want %v", tc.findings, tc.threshold, got, tc.want)
			}
		})
	}
}

// TestShouldFail_UnknownThresholdGatesOnError documents the fallback. It is
// deliberately conservative — an unrecognised threshold gates on error rather
// than gating on nothing — but it is a LAST resort, not the guard: every entry
// point rejects an invalid threshold before reaching here (see
// TestIsValidFailOn). If that validation is ever removed, this is what a typo
// would silently do.
func TestShouldFail_UnknownThresholdGatesOnError(t *testing.T) {
	if !shouldFail(findingsAt(models.SeverityError), "warnings") {
		t.Error("unknown threshold must not gate on nothing")
	}
	if shouldFail(findingsAt(models.SeverityWarning), "warnings") {
		t.Error("unknown threshold should behave as error-gating, not warning-gating")
	}
}

// TestIsValidFailOn is the guard that keeps the fallback above unreachable in
// practice. "none" is listed explicitly: the GitHub Action documents it as a
// report-only mode and implements that itself, so the CLI must reject it
// rather than silently gate on errors.
func TestIsValidFailOn(t *testing.T) {
	for _, ok := range []string{"error", "warning", "info"} {
		if !isValidFailOn(ok) {
			t.Errorf("isValidFailOn(%q) = false, want true", ok)
		}
	}
	for _, bad := range []string{"none", "", "warnings", "ERROR", "Error", "high", "critical", " error"} {
		if isValidFailOn(bad) {
			t.Errorf("isValidFailOn(%q) = true, want false", bad)
		}
	}
}

// TestCountAtOrAbove mirrors shouldFail's ranking; the two are reported
// side by side in the gate summary, so they must agree about what "at or
// above" means.
func TestCountAtOrAbove(t *testing.T) {
	all := findingsAt(
		models.SeverityError, models.SeverityError,
		models.SeverityWarning, models.SeverityWarning, models.SeverityWarning,
		models.SeverityInfo,
	)
	cases := []struct {
		min  string
		want int
	}{
		{"error", 2},
		{"warning", 5},
		{"info", 6},
	}
	for _, tc := range cases {
		if got := countAtOrAbove(all, tc.min); got != tc.want {
			t.Errorf("countAtOrAbove(min=%q) = %d, want %d", tc.min, got, tc.want)
		}
	}
	if got := countAtOrAbove(nil, "error"); got != 0 {
		t.Errorf("countAtOrAbove(nil) = %d, want 0", got)
	}
}

// TestCountAtOrAbove_AgreesWithShouldFail pins the relationship the gate
// summary implies: "N findings at or above X" is non-zero exactly when the
// gate trips at X.
func TestCountAtOrAbove_AgreesWithShouldFail(t *testing.T) {
	defer flipToPreview(t, "missing-timeout")()
	preview := []models.Finding{{RuleID: "missing-timeout", Severity: models.SeverityError}}
	sets := [][]models.Finding{
		nil,
		findingsAt(models.SeverityInfo),
		findingsAt(models.SeverityWarning),
		findingsAt(models.SeverityError),
		findingsAt(models.SeverityInfo, models.SeverityWarning),
		findingsAt(models.SeverityWarning, models.SeverityError),
		findingsAt(models.Severity("critical")),
		findingsAt(models.SeverityInfo, models.Severity("")),
		preview,
		append(findingsAt(models.SeverityInfo), preview...),
	}
	for _, threshold := range []string{"error", "warning", "info"} {
		for i, set := range sets {
			gate := shouldFail(set, threshold)
			count := countAtOrAbove(set, threshold)
			if gate != (count > 0) {
				t.Errorf("set %d at %q: shouldFail=%v but countAtOrAbove=%d", i, threshold, gate, count)
			}
		}
	}
}

// TestParseRuleSet covers the -rule/-rules parsing every gate path funnels
// through. nil (not an empty map) is the "no filter" signal — an empty map
// would filter everything out.
func TestParseRuleSet(t *testing.T) {
	if got := parseRuleSet(""); got != nil {
		t.Errorf("empty input must yield nil (no filter), got %v", got)
	}
	got := parseRuleSet(" missing-timeout , unhandled-error ,, ")
	want := map[string]bool{"missing-timeout": true, "unhandled-error": true}
	if len(got) != len(want) {
		t.Fatalf("parsed %v, want %v", got, want)
	}
	for id := range want {
		if !got[id] {
			t.Errorf("missing %q in %v", id, got)
		}
	}
}

func TestFirstNonEmptyStr(t *testing.T) {
	cases := []struct{ a, b, want string }{
		{"a", "b", "a"},
		{"", "b", "b"},
		{"   ", "b", "b"},
		{"\t\n", "b", "b"},
		{"", "", ""},
	}
	for _, tc := range cases {
		if got := firstNonEmptyStr(tc.a, tc.b); got != tc.want {
			t.Errorf("firstNonEmptyStr(%q, %q) = %q, want %q", tc.a, tc.b, got, tc.want)
		}
	}
}

func TestApplySuffix(t *testing.T) {
	if got := applySuffix(true, "Main.txt"); got != " → wrote Main.txt" {
		t.Errorf("applied: got %q", got)
	}
	if got := applySuffix(false, "Main.txt"); got == "" || got == " → wrote Main.txt" {
		t.Errorf("dry-run must not claim a write: %q", got)
	}
}

// TestBuildSettingsFromConfig covers the translation from .bakirc.json into the
// settings the analyzer reads — the layer that decides whether a rule runs.
func TestBuildSettingsFromConfig(t *testing.T) {
	if got := buildSettingsFromConfig(nil); got != nil {
		t.Errorf("nil config must yield nil settings (all rules default), got %+v", got)
	}
	if got := buildSettingsFromConfig(&bakiConfig{}); got != nil {
		t.Errorf("config with no ruleConfig must yield nil settings, got %+v", got)
	}

	cfg := &bakiConfig{RuleCfg: map[string]bakiRuleConf{
		"a": {Enabled: boolPtr(false)},
		"b": {Severity: "warning"},
		"c": {Enabled: boolPtr(true), Severity: "info"},
	}}
	s := buildSettingsFromConfig(cfg)
	if s == nil {
		t.Fatal("expected settings")
	}
	if rc := s.Analysis.Rules["a"]; rc.Enabled {
		t.Error("rule a: explicit false must disable")
	}
	// Omitting `enabled` means "leave it on", not "turn it off" — the opposite
	// would silently disable every rule a config only sets a severity for.
	if rc := s.Analysis.Rules["b"]; !rc.Enabled || rc.Severity != "warning" {
		t.Errorf("rule b: want enabled with severity warning, got %+v", rc)
	}
	if rc := s.Analysis.Rules["c"]; !rc.Enabled || rc.Severity != "info" {
		t.Errorf("rule c: got %+v", rc)
	}
}

// TestBuildSettingsFromConfig_Thresholds: rule thresholds (maxDepth,
// minRepeats, maxBlocks) ride along from the config file. They used to have no
// field at all, so a threshold in .bakirc.json decoded to nothing and the rule
// silently kept its default.
func TestBuildSettingsFromConfig_Thresholds(t *testing.T) {
	cfg := &bakiConfig{RuleCfg: map[string]bakiRuleConf{
		"deep-nesting": {Options: map[string]any{"maxDepth": 2.0}},
		"plain":        {Severity: "warning"},
	}}
	s := buildSettingsFromConfig(cfg)
	if s == nil {
		t.Fatal("expected settings")
	}
	rc := s.Analysis.Rules["deep-nesting"]
	if !rc.Enabled {
		t.Error("an options-only entry must leave the rule enabled")
	}
	if got := rc.Options["maxDepth"]; got != 2.0 {
		t.Errorf("maxDepth dropped in translation: got %v", got)
	}
	if s.Analysis.Rules["plain"].Options != nil {
		t.Error("a rule without options must not grow an empty map")
	}
	// The settings must not alias the config's map: the analyzer hands
	// settings to rules that may run concurrently with a config reload.
	cfg.RuleCfg["deep-nesting"].Options["maxDepth"] = 9.0
	if got := s.Analysis.Rules["deep-nesting"].Options["maxDepth"]; got != 2.0 {
		t.Errorf("settings alias the config map (got %v after mutating the source)", got)
	}
}

// TestBuildDefaultConfig_CarriesThresholds: `bakicli init` writes the
// thresholds the rules actually honour, so a user discovers that maxDepth /
// minRepeats / maxBlocks exist by reading the generated file rather than the
// source.
func TestBuildDefaultConfig_CarriesThresholds(t *testing.T) {
	cfg := buildDefaultConfig()
	for id, key := range map[string]string{
		"deep-nesting":     "maxDepth",
		"duplicate-action": "minRepeats",
		"wide-loop":        "maxBlocks",
		"large-subflow":    "maxBlocks",
	} {
		rc, ok := cfg.RuleCfg[id]
		if !ok {
			t.Fatalf("default config has no entry for %s", id)
		}
		if _, ok := rc.Options[key]; !ok {
			t.Errorf("%s: init output does not document its %s threshold", id, key)
		}
	}
	if len(cfg.RuleCfg["unhandled-error"].Options) != 0 {
		t.Error("a rule without thresholds must not get an options block")
	}
}

// flipToPreview moves a built-in rule to the preview stage until restore runs.
// The meta table is process-global: never from a parallel test.
func flipToPreview(t *testing.T, ruleID string) (restore func()) {
	t.Helper()
	m, ok := analyzer.LookupMeta(ruleID)
	if !ok {
		t.Fatalf("no built-in rule %q", ruleID)
	}
	m.Stage = analyzer.StagePreview
	return analyzer.SetRuleMetaForTesting(ruleID, m)
}

// TestShouldFail_PreviewNeverGates: a preview finding (stamped, or unstamped
// from a rule that is preview now) trips no threshold and is not counted, an
// unknown severity included; a stable finding beside it still gates.
func TestShouldFail_PreviewNeverGates(t *testing.T) {
	defer flipToPreview(t, "missing-timeout")()
	previewOnly := []models.Finding{
		{RuleID: "missing-timeout", Severity: models.SeverityError},                     // unstamped, preview rule
		{RuleID: "org-rule", Severity: models.SeverityError, Stage: "preview"},          // stamped
		{RuleID: "missing-timeout", Severity: models.Severity("critical")},              // preview, unknown severity
		{RuleID: "unhandled-error", Severity: models.SeverityWarning, Stage: "preview"}, // stamped stable rule
	}
	for _, threshold := range []string{"error", "warning", "info"} {
		if shouldFail(previewOnly, threshold) {
			t.Errorf("preview findings tripped -fail-on %s", threshold)
		}
		if n := countAtOrAbove(previewOnly, threshold); n != 0 {
			t.Errorf("countAtOrAbove(preview, %s) = %d, want 0", threshold, n)
		}
	}
	mixed := append([]models.Finding{{RuleID: "unhandled-error", Severity: models.SeverityError}}, previewOnly...)
	if !shouldFail(mixed, "error") || countAtOrAbove(mixed, "error") != 1 {
		t.Errorf("the stable error beside preview findings must gate alone: shouldFail=%v count=%d",
			shouldFail(mixed, "error"), countAtOrAbove(mixed, "error"))
	}
	if !shouldFail([]models.Finding{{RuleID: "unhandled-error", Severity: "critical"}}, "error") {
		t.Error("a stable finding with an unknown severity must still fail closed")
	}
}

// TestCountAtOrAbove_UnknownSeverityFailsClosed: the count used in the gate
// message used to drop an unknown severity while shouldFail failed on it, so
// "gate FAIL: 0 added finding(s)" was printable.
func TestCountAtOrAbove_UnknownSeverityFailsClosed(t *testing.T) {
	fs := findingsAt(models.Severity("critical"), models.SeverityInfo)
	for threshold, want := range map[string]int{"error": 1, "warning": 1, "info": 2} {
		if got := countAtOrAbove(fs, threshold); got != want {
			t.Errorf("countAtOrAbove(min=%q) = %d, want %d", threshold, got, want)
		}
	}
}

func TestPreviewNote(t *testing.T) {
	defer flipToPreview(t, "missing-timeout")()
	if got := previewNote(findingsAt(models.SeverityError)); got != "" {
		t.Errorf("no preview finding: %q, want empty (text output unchanged)", got)
	}
	one := []models.Finding{{RuleID: "missing-timeout", Severity: models.SeverityWarning}}
	if got := previewNote(one); got != " (1 preview finding not gated)" {
		t.Errorf("one: %q", got)
	}
	two := append(one, models.Finding{RuleID: "x", Stage: "preview"})
	if got := previewNote(two); got != " (2 preview findings not gated)" {
		t.Errorf("two: %q", got)
	}
}
