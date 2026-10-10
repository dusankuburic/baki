package service

import (
	"context"
	"testing"

	"pad-core/analyzer"
	"pad-core/models"
)

func catalogByID(rules []models.Rule) map[string]models.Rule {
	out := make(map[string]models.Rule, len(rules))
	for _, r := range rules {
		out[r.ID] = r
	}
	return out
}

// TestGetRules_MetadataFromRuleMeta: the catalog reads confidence, fix, stage,
// rule-set version and kind from the rule metadata (MetaOf), so direct fixes and
// custom rules are covered, not only the engine-stamped built-in fixes.
func TestGetRules_MetadataFromRuleMeta(t *testing.T) {
	custom, err := analyzer.NewCustomRule(analyzer.CustomRuleConfig{
		ID: "org-no-delay", Name: "No delay", Severity: "warning", Category: "Reliability",
		RawTypeMatch: "^WAIT$", AutoFix: "insert-delay",
	})
	if err != nil {
		t.Fatalf("NewCustomRule: %v", err)
	}
	svc, err := NewAnalysisService(NilNotifier{}, newTestSettingsStore(t), nil, AnalysisOptions{
		CustomRules: DeploymentCustomRules{custom},
	})
	if err != nil {
		t.Fatalf("NewAnalysisService: %v", err)
	}
	byID := catalogByID(svc.GetRules())

	for _, r := range analyzer.AllRules() {
		got, ok := byID[r.ID()]
		if !ok {
			t.Errorf("%s missing from the catalog", r.ID())
			continue
		}
		m := analyzer.MetaOf(r)
		if got.Confidence != m.Confidence || got.Stage != string(m.Stage) || got.IntroducedIn != m.IntroducedIn ||
			got.Kind != analyzer.RuleKind(r) {
			t.Errorf("%s: catalog {%s %q %d %q}, meta {%s %q %d %q}", r.ID(), got.Confidence, got.Stage,
				got.IntroducedIn, got.Kind, m.Confidence, m.Stage, m.IntroducedIn, analyzer.RuleKind(r))
		}
		if got.Stage != "stable" || got.IntroducedIn != 1 {
			t.Errorf("%s: stage %q, introducedIn %d; want stable, 1", r.ID(), got.Stage, got.IntroducedIn)
		}
	}
	if got := byID["circular-subflow-dependency"].Kind; got != analyzer.RuleKindFlow {
		t.Errorf("circular-subflow-dependency kind = %q, want flow", got)
	}
	if got := byID["unhandled-error"].Kind; got != analyzer.RuleKindBlock {
		t.Errorf("unhandled-error kind = %q, want block", got)
	}
	// subflow-mismatch stamps append-output itself; the catalog used to show no fix for it.
	if got := byID["subflow-mismatch"].AutoFix; got != "append-output" {
		t.Errorf("subflow-mismatch autoFix = %q, want the direct fix append-output", got)
	}
	if got := byID["unhandled-error"].AutoFix; got != "wrap-error-handler" {
		t.Errorf("unhandled-error autoFix = %q, want wrap-error-handler", got)
	}

	c := byID["org-no-delay"]
	if c.AutoFix != "insert-delay" || c.Confidence != models.ConfidenceMedium || c.Stage != "stable" ||
		c.IntroducedIn != 0 || c.Kind != analyzer.RuleKindBlock || !c.Enabled {
		t.Errorf("custom rule entry = %+v; want its own fix, medium, stable, no rule-set version, block, enabled", c)
	}
}

// TestGetRules_EnabledAndStageFollowTheEngine: a rule with no settings entry is
// enabled exactly when the engine runs it (RuleEnabled: !DisabledByDefault), an
// entry wins, and a stage flip reaches the catalog.
func TestGetRules_EnabledAndStageFollowTheEngine(t *testing.T) {
	restore := analyzer.SetRuleMetaForTesting("deep-nesting", analyzer.RuleMeta{
		Confidence: models.ConfidenceLow, IntroducedIn: 1, Stage: analyzer.StagePreview, DisabledByDefault: true,
	})
	defer restore()

	s := models.DefaultSettings()
	delete(s.Analysis.Rules, "deep-nesting")
	svc := &AnalysisService{settings: &staticSettings{s: *s}}
	got := catalogByID(svc.GetRules())["deep-nesting"]
	if got.Enabled {
		t.Error("absent entry of a DisabledByDefault rule: catalog says enabled, the engine does not run it")
	}
	if got.Stage != "preview" || got.Confidence != models.ConfidenceLow {
		t.Errorf("stage/confidence = %q/%s, want the flipped meta preview/low", got.Stage, got.Confidence)
	}

	s.Analysis.Rules["deep-nesting"] = models.RuleConfig{Enabled: true, Severity: "info"}
	svc = &AnalysisService{settings: &staticSettings{s: *s}}
	if !catalogByID(svc.GetRules())["deep-nesting"].Enabled {
		t.Error("an explicit enabled entry must win over DisabledByDefault")
	}

	// No settings at all (nil provider): the same defaults.
	if catalogByID((&AnalysisService{}).GetRules())["deep-nesting"].Enabled {
		t.Error("nil settings: a DisabledByDefault rule must read disabled")
	}
}

// TestGetRules_LibraryRulesOnlyWhereTheyRun: the library rules run only in the
// desktop folder analysis, so only a deployment that runs them lists them, as
// preview library rules that settings can toggle; the per-flow catalog and its
// summary are unchanged elsewhere.
func TestGetRules_LibraryRulesOnlyWhereTheyRun(t *testing.T) {
	lib := analyzer.LibraryRules()
	if len(lib) == 0 {
		t.Fatal("no library rule registered")
	}
	for _, runs := range []bool{false, true} {
		svc, err := NewAnalysisService(NilNotifier{}, newTestSettingsStore(t), nil, AnalysisOptions{LibraryRules: runs})
		if err != nil {
			t.Fatalf("NewAnalysisService: %v", err)
		}
		rules := svc.GetRules()
		byID := catalogByID(rules)
		want := len(analyzer.AllRules())
		if runs {
			want += len(lib)
		}
		if len(rules) != want {
			t.Errorf("runs=%v: %d catalog entries, want %d", runs, len(rules), want)
		}
		if s := svc.GetRulesSummary(); s.TotalRules != want {
			t.Errorf("runs=%v: summary counts %d rules, want %d", runs, s.TotalRules, want)
		}
		for _, r := range lib {
			got, listed := byID[r.ID()]
			if listed != runs {
				t.Errorf("runs=%v: library rule %s listed = %v", runs, r.ID(), listed)
				continue
			}
			if !runs {
				continue
			}
			if got.Kind != analyzer.RuleKindLibrary || got.Stage != "preview" || !got.Enabled || got.Options != nil {
				t.Errorf("library entry = %+v; want kind library, stage preview, enabled, no options", got)
			}
		}
		// Library entries come after the per-flow rules (the catalog order the UI groups by).
		if runs && rules[len(rules)-1].Kind != analyzer.RuleKindLibrary {
			t.Errorf("last entry %s is not a library rule", rules[len(rules)-1].ID)
		}
		if runs {
			id := lib[0].ID()
			if err := svc.SetRuleEnabled(context.Background(), id, false); err != nil {
				t.Fatalf("SetRuleEnabled: %v", err)
			}
			if catalogByID(svc.GetRules())[id].Enabled {
				t.Errorf("%s still enabled after the toggle", id)
			}
		}
	}
}
