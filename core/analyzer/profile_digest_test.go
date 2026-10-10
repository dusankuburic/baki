package analyzer

import (
	"encoding/json"
	"slices"
	"testing"

	"pad-core/models"
)

func ruleSettings(rules map[string]models.RuleConfig) *models.AppSettings {
	return &models.AppSettings{Analysis: models.AnalysisSettings{Rules: rules}}
}

// TestProfileDigest_WhatChangesIt: everything a report depends on besides the document changes the digest, and
// nothing else does.
func TestProfileDigest_WhatChangesIt(t *testing.T) {
	builtins := AllRules()
	base := ProfileDigest(models.DefaultSettings(), builtins)
	if len(base) != 64 {
		t.Fatalf("digest %q: want a full SHA-256 in hex", base)
	}
	for range 20 { // map iteration order must not leak in
		if ProfileDigest(models.DefaultSettings(), builtins) != base {
			t.Fatal("digest not deterministic")
		}
	}
	reversed := slices.Clone(builtins)
	slices.Reverse(reversed)
	if ProfileDigest(models.DefaultSettings(), reversed) != base {
		t.Error("rule order changed the digest")
	}

	differs := func(name string, settings *models.AppSettings, rules []Rule) {
		t.Helper()
		if ProfileDigest(settings, rules) == base {
			t.Errorf("%s did not change the digest", name)
		}
	}
	disabled := models.DefaultSettings()
	rc := disabled.Analysis.Rules["deep-nesting"]
	rc.Enabled = false
	disabled.Analysis.Rules["deep-nesting"] = rc
	differs("disabling a rule", disabled, builtins)

	severity := models.DefaultSettings()
	rc = severity.Analysis.Rules["deep-nesting"]
	rc.Severity = "error"
	severity.Analysis.Rules["deep-nesting"] = rc
	differs("a severity override", severity, builtins)

	differs("removing a rule", models.DefaultSettings(), builtins[1:])
	differs("nil settings", nil, builtins)
	differs("adding a custom rule", models.DefaultSettings(), append(slices.Clone(builtins), mustCustom(t, "house", "^SET$")))
	if ProfileDigest(nil, []Rule{mustCustom(t, "house", "^SET$")}) == ProfileDigest(nil, []Rule{mustCustom(t, "house", "^WAIT$")}) {
		t.Error("two configurations under one custom rule id share a digest")
	}
}

// B5: options are folded with their type. The rules read only numbers, so "8" is not 8.
func TestProfileDigest_OptionTypes(t *testing.T) {
	with := func(v any) string {
		return ProfileDigest(ruleSettings(map[string]models.RuleConfig{
			"deep-nesting": {Enabled: true, Severity: "warning", Options: map[string]interface{}{"maxDepth": v}},
		}), AllRules())
	}
	seen := map[string]any{}
	for _, v := range []any{"8", 8, 8.5, int64(8), json.Number("8"), true, nil, []any{8}, map[string]any{"a": "8"}, map[string]any{"a": 8}} {
		d := with(v)
		if prev, dup := seen[d]; dup {
			t.Errorf("options %#v and %#v share a digest", prev, v)
		}
		seen[d] = v
	}
	// Settings decoded from JSON carry float64: equal to the same value built in Go.
	var decoded map[string]models.RuleConfig
	if err := json.Unmarshal([]byte(`{"deep-nesting":{"enabled":true,"severity":"warning","options":{"maxDepth":8}}}`), &decoded); err != nil {
		t.Fatal(err)
	}
	if ProfileDigest(ruleSettings(decoded), AllRules()) != with(float64(8)) {
		t.Error("equal options digest differently")
	}
}

// A rule's metadata is stamped on its findings, so a flip of its stage, revision, confidence or fix type changes
// the digest, as does DisabledByDefault for a rule the settings do not mention; the parse-error fallback counts
// even when parse-error is not in the set. Restoring the metadata restores the digest.
func TestProfileDigest_FoldsRuleMetadata(t *testing.T) {
	rules := []Rule{&DeepNestingRule{}}
	settings := ruleSettings(map[string]models.RuleConfig{})
	base := ProfileDigest(settings, rules)
	orig, ok := LookupMeta("deep-nesting")
	if !ok {
		t.Fatal("deep-nesting has no metadata")
	}
	for name, edit := range map[string]func(*RuleMeta){
		"stage":               func(m *RuleMeta) { m.Stage = StagePreview },
		"revision":            func(m *RuleMeta) { m.Revision = 7 },
		"confidence":          func(m *RuleMeta) { m.Confidence = otherConfidence(m.Confidence) },
		"fix type":            func(m *RuleMeta) { m.AutoFix = "suppress" },
		"disabled by default": func(m *RuleMeta) { m.DisabledByDefault = true },
	} {
		m := orig
		edit(&m)
		restore := SetRuleMetaForTesting("deep-nesting", m)
		if ProfileDigest(settings, rules) == base {
			t.Errorf("a %s flip did not change the digest", name)
		}
		restore()
		if ProfileDigest(settings, rules) != base {
			t.Errorf("restoring the %s did not restore the digest", name)
		}
	}

	pe, _ := LookupMeta("parse-error")
	pe.Stage = StagePreview
	restore := SetRuleMetaForTesting("parse-error", pe)
	defer restore()
	if ProfileDigest(settings, rules) == base {
		t.Error("the parse-error fallback's stage is not folded")
	}
}

func mustCustom(t *testing.T, id, rawTypeMatch string) Rule {
	t.Helper()
	r, err := NewCustomRule(CustomRuleConfig{ID: id, Name: id, Severity: "warning", Category: "Style", RawTypeMatch: rawTypeMatch})
	if err != nil {
		t.Fatal(err)
	}
	return r
}

func otherConfidence(c models.Confidence) models.Confidence {
	if c == models.ConfidenceLow {
		return models.ConfidenceHigh
	}
	return models.ConfidenceLow
}
