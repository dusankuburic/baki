package service

import (
	"testing"

	"pad-core/models"
)

// TestGetRules_SurfacesThresholds: the rule catalog must carry each rule's
// numeric thresholds — the configured override when there is one, else the
// built-in default — so the settings UI can render them. They used to be
// invisible: reachable only by hand-editing the settings JSON.
func TestGetRules_SurfacesThresholds(t *testing.T) {
	s := models.DefaultSettings()
	s.Analysis.Rules["deep-nesting"] = models.RuleConfig{Enabled: true, Severity: "info", Options: map[string]any{"maxDepth": 3.0}}
	svc := &AnalysisService{settings: &staticSettings{s: *s}}
	byID := map[string]models.Rule{}
	for _, r := range svc.GetRules() {
		byID[r.ID] = r
	}
	if got := byID["deep-nesting"].Options["maxDepth"]; got != 3.0 {
		t.Errorf("configured maxDepth not surfaced: %v", byID["deep-nesting"].Options)
	}
	if got := byID["wide-loop"].Options["maxBlocks"]; got != 20 {
		t.Errorf("default maxBlocks not surfaced for an unconfigured rule: %v", byID["wide-loop"].Options)
	}
	if byID["unhandled-error"].Options != nil {
		t.Errorf("a rule without thresholds must not get options: %v", byID["unhandled-error"].Options)
	}
}
