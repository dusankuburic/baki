package analyzer

import (
	"testing"

	"pad-core/models"
)

// optionsFixture trips missing-timeout (and a couple of other auto-fixable
// rules) on a single HTTP action.
const optionsFixture = "#Region \"Main\"\n" +
	"HTTPClient.InvokeUrl Method: GET Url: 'https://api.example.com/x'\n" +
	"#EndRegion\n"

func fixedRules(t *testing.T, opts FixOptions) map[string]int {
	t.Helper()
	seen := map[string]int{}
	opts.OnFix = func(ruleID, _ string, _ int) { seen[ruleID]++ }
	src := optionsFixture
	if _, err := ApplyFixesToSource(&src, "Main.txt", opts); err != nil {
		t.Fatalf("ApplyFixesToSource: %v", err)
	}
	return seen
}

// TestFixOptions_DisabledRuleIsNotFixed is the loop's half of the contract: the
// caller's settings gate which rules run, so a disabled rule cannot be fixed.
// The loop used to hardcode models.DefaultSettings(), which made it disagree
// with the analysis that produced the findings — it would rewrite a flow to
// resolve a finding the caller's configuration had switched off.
func TestFixOptions_DisabledRuleIsNotFixed(t *testing.T) {
	if base := fixedRules(t, FixOptions{Limit: 50}); base["missing-timeout"] == 0 {
		t.Fatalf("fixture has rotted: missing-timeout not fixed at default settings (applied: %v)", base)
	}

	settings := models.DefaultSettings()
	if settings.Analysis.Rules == nil {
		settings.Analysis.Rules = map[string]models.RuleConfig{}
	}
	settings.Analysis.Rules["missing-timeout"] = models.RuleConfig{Enabled: false}

	got := fixedRules(t, FixOptions{Settings: settings, Limit: 50})
	if got["missing-timeout"] != 0 {
		t.Errorf("applied %d missing-timeout fix(es) for a disabled rule (applied: %v)", got["missing-timeout"], got)
	}
	if len(got) == 0 {
		t.Error("disabling one rule silenced the whole loop; the others must still fix")
	}
}

// TestFixOptions_NilSettingsKeepsDefaults pins the zero-value contract every
// existing caller relies on: no Settings means the built-in defaults, not "no
// settings" (which would leave severities and options unresolved).
func TestFixOptions_NilSettingsKeepsDefaults(t *testing.T) {
	withNil := fixedRules(t, FixOptions{Limit: 50})
	withDefaults := fixedRules(t, FixOptions{Settings: models.DefaultSettings(), Limit: 50})
	if len(withNil) != len(withDefaults) {
		t.Fatalf("nil Settings diverged from DefaultSettings: %v vs %v", withNil, withDefaults)
	}
	for id, n := range withDefaults {
		if withNil[id] != n {
			t.Errorf("rule %q: nil Settings applied %d fixes, DefaultSettings applied %d", id, withNil[id], n)
		}
	}
}

// TestFixOptions_RulesReplacesTheSetRatherThanExtendingIt guards the semantic
// that makes an org profile (or the CLI's selectRules) safe to pass straight
// in: Rules is the COMPLETE set. Treating it as "extras appended to the
// built-ins" would dispatch every built-in twice.
func TestFixOptions_RulesReplacesTheSetRatherThanExtendingIt(t *testing.T) {
	var only []Rule
	for _, r := range AllRules() {
		if r.ID() == "missing-timeout" {
			only = append(only, r)
		}
	}
	if len(only) != 1 {
		t.Fatalf("expected exactly one missing-timeout rule, got %d", len(only))
	}

	got := fixedRules(t, FixOptions{Rules: only, Limit: 50})
	if got["missing-timeout"] == 0 {
		t.Errorf("the one supplied rule was not run (applied: %v)", got)
	}
	for id := range got {
		if id != "missing-timeout" {
			t.Errorf("Rules must be the complete set, but %q ran too (applied: %v)", id, got)
		}
	}
}

// TestFixOptions_ZeroLimitAppliesNothing documents the Limit contract — the
// loop body is bounded by it, so a caller that forgets to set it gets a no-op
// rather than an unbounded rewrite.
func TestFixOptions_ZeroLimitAppliesNothing(t *testing.T) {
	src := optionsFixture
	n, err := ApplyFixesToSource(&src, "Main.txt", FixOptions{})
	if err != nil {
		t.Fatalf("ApplyFixesToSource: %v", err)
	}
	if n != 0 {
		t.Errorf("Limit 0 applied %d fixes", n)
	}
	if src != optionsFixture {
		t.Error("Limit 0 mutated the source")
	}
}
