package service

import (
	"testing"

	"pad-core/models"
)

// TestRuleConfig_ToggleDoesNotWipeOverrides is the B1 regression test for the
// rule-config wipe chain: GetRules must surface the user's configured severity
// (not the built-in default), and UpdateRuleConfig must not erase configured
// Options when a client sends only {enabled, severity}.
func TestRuleConfig_ToggleDoesNotWipeOverrides(t *testing.T) {
	svc, err := NewAnalysisService(NilNotifier{}, newTestSettingsStore(t), nil)
	if err != nil {
		t.Fatalf("NewAnalysisService: %v", err)
	}

	// User configures a severity override + a threshold option.
	if err := svc.UpdateRuleConfig("deep-nesting", models.RuleConfig{
		Enabled:  true,
		Severity: "error",
		Options:  map[string]any{"maxDepth": 3},
	}); err != nil {
		t.Fatalf("UpdateRuleConfig: %v", err)
	}

	// GetRules must reflect the override, not the built-in default ("info").
	var got *models.Rule
	for _, r := range svc.GetRules() {
		if r.ID == "deep-nesting" {
			rc := r
			got = &rc
			break
		}
	}
	if got == nil {
		t.Fatal("deep-nesting rule not returned by GetRules")
	}
	if got.DefaultSeverity != models.Severity("error") {
		t.Errorf("GetRules severity = %q, want configured override %q", got.DefaultSeverity, "error")
	}

	// A toggle-style save (no Options, as the UI sends) must preserve them.
	if err := svc.UpdateRuleConfig("deep-nesting", models.RuleConfig{
		Enabled:  false,
		Severity: "error",
	}); err != nil {
		t.Fatalf("UpdateRuleConfig (toggle): %v", err)
	}

	rc := svc.settings.Get().Analysis.Rules["deep-nesting"]
	if rc.Enabled {
		t.Error("rule should be disabled after toggle")
	}
	if rc.Severity != "error" {
		t.Errorf("severity = %q after toggle, want %q preserved", rc.Severity, "error")
	}
	if rc.Options == nil || rc.Options["maxDepth"] != 3 {
		t.Errorf("Options = %v after toggle, want maxDepth=3 preserved", rc.Options)
	}

	// SetRuleEnabled (the dedicated toggle endpoint) must also preserve both.
	if err := svc.SetRuleEnabled("deep-nesting", true); err != nil {
		t.Fatalf("SetRuleEnabled: %v", err)
	}
	rc = svc.settings.Get().Analysis.Rules["deep-nesting"]
	if !rc.Enabled || rc.Severity != "error" || rc.Options == nil || rc.Options["maxDepth"] != 3 {
		t.Errorf("after SetRuleEnabled: %+v — want enabled with severity+options preserved", rc)
	}
}
