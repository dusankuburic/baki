package main

import (
	"testing"

	"pad-core/analyzer"
	"pad-core/models"
)

// fixConfigSource is a flow whose HTTP action trips missing-timeout,
// missing-retry and unhandled-error — three rules that all carry an auto-fix,
// so the loop has something to do under any configuration.
const fixConfigSource = "#Region \"Main\"\n" +
	"HTTPClient.InvokeUrl Method: GET Url: 'https://api.example.com/x'\n" +
	"#EndRegion\n"

func boolPtr(b bool) *bool { return &b }

// appliedRules runs the fix loop and reports which rules it applied a fix for.
func appliedRules(t *testing.T, source string, opts analyzer.FixOptions) map[string]int {
	t.Helper()
	seen := map[string]int{}
	opts.OnFix = func(ruleID, _ string, _ int) { seen[ruleID]++ }
	src := source
	if _, err := analyzer.ApplyFixesToSource(&src, "Main.txt", opts); err != nil {
		t.Fatalf("ApplyFixesToSource: %v", err)
	}
	return seen
}

// TestFix_ConfigDisabledRuleIsNotFixed is the regression gate for the bug where
// `bakicli fix` ignored .bakirc.json entirely: `analyze` suppressed a disabled
// rule while `fix` went on rewriting the flow to resolve its findings. The two
// commands must agree — a rule the config switches off is neither reported nor
// auto-fixed.
func TestFix_ConfigDisabledRuleIsNotFixed(t *testing.T) {
	// Baseline: with no config the rule is fixable, so a later "not fixed"
	// assertion means the config did it, not that the fixture stopped firing.
	base := appliedRules(t, fixConfigSource, buildFixOptions(nil, "", "", 50))
	if base["missing-timeout"] == 0 {
		t.Fatalf("fixture has rotted: missing-timeout was not fixed without a config (applied: %v)", base)
	}

	cfg := &bakiConfig{RuleCfg: map[string]bakiRuleConf{
		"missing-timeout": {Enabled: boolPtr(false)},
	}}
	opts := buildFixOptions(cfg, "", "", 50)
	if rc, ok := opts.Settings.Analysis.Rules["missing-timeout"]; !ok || rc.Enabled {
		t.Fatalf("buildFixOptions did not carry the config's disable into Settings: %+v", opts.Settings)
	}
	got := appliedRules(t, fixConfigSource, opts)
	if got["missing-timeout"] != 0 {
		t.Errorf("fix applied %d missing-timeout fix(es) for a rule the config disabled (applied: %v)",
			got["missing-timeout"], got)
	}
	// The disable must be surgical: other auto-fixable rules still run.
	if len(got) == 0 {
		t.Error("disabling one rule silenced the whole loop; expected the other rules to still fix")
	}
}

// TestFix_ConfigRuleSelectionNarrowsFixes verifies the config's `rules`
// selection reaches the fix loop when -rule is not given, so a config that
// narrows analysis to one rule narrows fixing the same way.
func TestFix_ConfigRuleSelectionNarrowsFixes(t *testing.T) {
	cfg := &bakiConfig{Rules: "missing-timeout"}
	got := appliedRules(t, fixConfigSource, buildFixOptions(cfg, "", "", 50))
	for id := range got {
		if id != "missing-timeout" {
			t.Errorf("config selected only missing-timeout but %q was fixed too (applied: %v)", id, got)
		}
	}
	// An explicit -rule still wins over the config.
	explicit := buildFixOptions(cfg, "unhandled-error", "", 50)
	if explicit.RuleFilter["missing-timeout"] {
		t.Error("-rule must override the config's rules selection, not merge with it")
	}
}

// TestFix_InlineCustomRuleWithAutoFixIsApplied pins the other half of the same
// bug: the loop ran the built-in rule set only, so a custom rule carrying an
// autoFix was reported by `analyze` and never applied by `fix`.
func TestFix_InlineCustomRuleWithAutoFixIsApplied(t *testing.T) {
	cfg := &bakiConfig{CustomRulesInline: []analyzer.CustomRuleConfig{{
		ID:           "team-http-timeout",
		Name:         "Team HTTP timeout",
		Description:  "HTTP actions must declare a timeout",
		Severity:     string(models.SeverityWarning),
		Category:     "reliability",
		RawTypeMatch: "HTTPClient.InvokeUrl",
		AutoFix:      "set-timeout",
	}}}
	opts := buildFixOptions(cfg, "", "", 50)

	var found bool
	for _, r := range opts.Rules {
		if r.ID() == "team-http-timeout" {
			found = true
		}
	}
	if !found {
		t.Fatalf("inline custom rule missing from the fix loop's rule set (%d rules)", len(opts.Rules))
	}
	if got := appliedRules(t, fixConfigSource, opts); got["team-http-timeout"] == 0 {
		t.Errorf("custom rule with an autoFix was never applied (applied: %v)", got)
	}
}

// TestFix_NoDisabledRuleIsEverFixed ties the two commands together by the
// property that actually holds across the whole run: no applied fix may belong
// to a rule the config disabled.
//
// It deliberately does NOT compare against a single analyze pass of the
// original source. The loop is iterative, so it legitimately fixes findings
// REVEALED by earlier fixes (wrap-error-handler inserts an empty handler, which
// error-swallow then reports) — those never appear in a first-pass report, and
// asserting otherwise would gate on a cascade that is working as designed.
func TestFix_NoDisabledRuleIsEverFixed(t *testing.T) {
	disabled := []string{"missing-timeout", "missing-retry", "unhandled-error"}
	ruleCfg := map[string]bakiRuleConf{}
	for _, id := range disabled {
		ruleCfg[id] = bakiRuleConf{Enabled: boolPtr(false)}
	}
	cfg := &bakiConfig{RuleCfg: ruleCfg}

	got := appliedRules(t, fixConfigSource, buildFixOptions(cfg, "", "", 50))
	for _, id := range disabled {
		if got[id] != 0 {
			t.Errorf("fix applied %d %q fix(es) for a rule the config disabled (applied: %v)", got[id], id, got)
		}
	}
}
