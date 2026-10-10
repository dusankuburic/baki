package analyzer

import (
	"encoding/json"
	"reflect"
	"slices"
	"sort"
	"strings"
	"testing"

	"pad-core/models"
)

// knownCategories are the categories rules file their findings under; the
// findings UI filters by them, so a new one must reach the UI too.
var knownCategories = []string{"Logic", "Performance", "Portability", "Reliability", "Security", "Style", "Syntax"}

// TestRuleMeta_EveryRuleIsComplete: every built-in declares its stage,
// rule-set version, revision and confidence explicitly (no implicit Medium).
func TestRuleMeta_EveryRuleIsComplete(t *testing.T) {
	for _, r := range AllRules() {
		m, ok := LookupMeta(r.ID())
		if !ok {
			t.Errorf("%s: no RuleMeta", r.ID())
			continue
		}
		if m.Stage != StageStable && m.Stage != StagePreview {
			t.Errorf("%s: Stage %q, want stable or preview", r.ID(), m.Stage)
		}
		if m.IntroducedIn < 1 || m.IntroducedIn > RuleSetVersion {
			t.Errorf("%s: IntroducedIn %d outside 1..RuleSetVersion (%d)", r.ID(), m.IntroducedIn, RuleSetVersion)
		}
		if m.Revision < 1 {
			t.Errorf("%s: Revision %d, want it set explicitly (1 = first revision)", r.ID(), m.Revision)
		}
		switch m.Confidence {
		case models.ConfidenceHigh, models.ConfidenceMedium, models.ConfidenceLow:
		default:
			t.Errorf("%s: Confidence %q, want high, medium or low", r.ID(), m.Confidence)
		}
	}
}

// TestRuleMeta_RuleSetVersionHasARule: a RuleSetVersion bump must ship with the
// rule that introduced it.
func TestRuleMeta_RuleSetVersionHasARule(t *testing.T) {
	maxIntroduced := 0
	for _, r := range AllRules() {
		maxIntroduced = max(maxIntroduced, MetaOf(r).IntroducedIn)
	}
	if maxIntroduced != RuleSetVersion {
		t.Errorf("highest IntroducedIn is %d, RuleSetVersion is %d", maxIntroduced, RuleSetVersion)
	}
}

// TestRuleMeta_FixReferencesAreRegistered: a rule can only name registered
// fixes, and a fix is either engine-stamped or direct, never both.
func TestRuleMeta_FixReferencesAreRegistered(t *testing.T) {
	for _, r := range AllRules() {
		m := MetaOf(r)
		if m.AutoFix != "" {
			if _, ok := LookupFix(m.AutoFix); !ok {
				t.Errorf("%s: AutoFix %q is not a registered fix", r.ID(), m.AutoFix)
			}
		}
		seen := map[string]bool{}
		for _, fx := range m.DirectFixes {
			if _, ok := LookupFix(fx); !ok {
				t.Errorf("%s: direct fix %q is not a registered fix", r.ID(), fx)
			}
			if fx == m.AutoFix {
				t.Errorf("%s: %q is both AutoFix and a direct fix", r.ID(), fx)
			}
			if seen[fx] {
				t.Errorf("%s: direct fix %q listed twice", r.ID(), fx)
			}
			seen[fx] = true
		}
	}
}

// TestFixRegistry_EveryFixIsReferenced: a registered fix no rule offers is dead
// code that looks like coverage, unless it is offered standalone (suppress).
func TestFixRegistry_EveryFixIsReferenced(t *testing.T) {
	used := map[string]bool{}
	for _, r := range AllRules() {
		m := MetaOf(r)
		used[m.AutoFix] = true
		for _, fx := range m.DirectFixes {
			used[fx] = true
		}
	}
	for _, spec := range FixTypes() {
		if !used[spec.Type] && spec.Flags&FixStandalone == 0 {
			t.Errorf("fix %q is registered but no rule offers it and it is not FixStandalone", spec.Type)
		}
	}
	if !sort.SliceIsSorted(FixTypes(), func(i, j int) bool { return FixTypes()[i].Type < FixTypes()[j].Type }) {
		t.Error("FixTypes is not sorted by Type")
	}
}

// TestRuleMeta_RelatedTargets: the editorial related-rules map names real,
// distinct rules once each, with a reason, and stays acyclic.
func TestRuleMeta_RelatedTargets(t *testing.T) {
	ids := map[string]bool{}
	for _, r := range AllRules() {
		ids[r.ID()] = true
	}
	pairs := map[[2]string]bool{}
	adj := map[string][]string{}
	for _, d := range RelatedRules() {
		if !ids[d.ToRuleID] {
			t.Errorf("%s: related rule %q is not registered", d.FromRuleID, d.ToRuleID)
		}
		if d.FromRuleID == d.ToRuleID {
			t.Errorf("%s: related to itself", d.FromRuleID)
		}
		if strings.TrimSpace(d.Reason) == "" {
			t.Errorf("%s -> %s: empty reason", d.FromRuleID, d.ToRuleID)
		}
		k := [2]string{d.FromRuleID, d.ToRuleID}
		if pairs[k] {
			t.Errorf("%s -> %s listed twice", d.FromRuleID, d.ToRuleID)
		}
		pairs[k] = true
		adj[d.FromRuleID] = append(adj[d.FromRuleID], d.ToRuleID)
	}
	if cycles := detectCycles(adj, ids); len(cycles) > 0 {
		t.Errorf("related-rules map has cycles: %v", cycles)
	}
}

// TestRelatedRules_Pinned pins the editorial map moved out of incremental.go
// into RuleMeta.Related, in RelatedRules' (from, to) order.
func TestRelatedRules_Pinned(t *testing.T) {
	want := []models.RuleDependency{
		{FromRuleID: "dead-code", ToRuleID: "unused-variable", Reason: "dead code may produce unused variable findings"},
		{FromRuleID: "deep-nesting", ToRuleID: "infinite-loop-risk", Reason: "deep nesting increases loop complexity"},
		{FromRuleID: "empty-branch", ToRuleID: "dead-code", Reason: "empty branches may indicate dead code paths"},
		{FromRuleID: "file-op-no-error-handler", ToRuleID: "unhandled-error", Reason: "file operations are a subset of error handling"},
		{FromRuleID: "goto-antipattern", ToRuleID: "infinite-loop-risk", Reason: "goto can create hidden loop structures"},
		{FromRuleID: "missing-retry", ToRuleID: "missing-timeout", Reason: "retry and timeout checks are related reliability concerns"},
		{FromRuleID: "sensitive-exposure", ToRuleID: "hardcoded-credential", Reason: "sensitive exposure may involve hardcoded credentials"},
		{FromRuleID: "subflow-no-error-handler", ToRuleID: "unhandled-error", Reason: "subflow error handling relates to general error handling"},
		{FromRuleID: "unhandled-error", ToRuleID: "error-swallow", Reason: "error handling must be checked before swallow detection"},
	}
	if got := RelatedRules(); !reflect.DeepEqual(got, want) {
		t.Errorf("RelatedRules() =\n%v\nwant\n%v", got, want)
	}
}

// TestRuleMeta_Options: option keys are unique per rule and their defaults
// respect their minimums.
func TestRuleMeta_Options(t *testing.T) {
	for _, r := range AllRules() {
		keys := map[string]bool{}
		for _, o := range MetaOf(r).Options {
			if o.Key == "" || keys[o.Key] {
				t.Errorf("%s: option key %q empty or repeated", r.ID(), o.Key)
			}
			keys[o.Key] = true
			if o.Default < o.Min {
				t.Errorf("%s: option %s default %d below its minimum %d", r.ID(), o.Key, o.Default, o.Min)
			}
		}
	}
}

// TestDefaultSettings_MirrorsRuleMeta: models.DefaultSettings keeps a literal
// copy of the rule defaults (core/models cannot import the analyzer). A rule
// whose DefaultSeverity disagreed with it made the CLI without a config and
// the server grade the same finding differently (hardcoded-credential,
// decision D2).
func TestDefaultSettings_MirrorsRuleMeta(t *testing.T) {
	got := models.DefaultSettings().Analysis.Rules
	want := DefaultRuleConfigs()
	for id, w := range want {
		g, ok := got[id]
		if !ok {
			t.Errorf("DefaultSettings lacks rule %q (want %+v)", id, w)
			continue
		}
		if !reflect.DeepEqual(g, w) {
			t.Errorf("DefaultSettings[%q] = %+v, RuleMeta and DefaultSeverity say %+v", id, g, w)
		}
	}
	for id := range got {
		if _, ok := want[id]; !ok {
			t.Errorf("DefaultSettings lists %q, which is not a registered rule", id)
		}
	}
}

// TestRuleMeta_FindingsUseDeclaredFixes runs the corpus and checks that every
// finding's fix is one its rule declares: a rule that stamps an undeclared fix
// in Check would bypass AutoFixableRules and the catalog.
func TestRuleMeta_FindingsUseDeclaredFixes(t *testing.T) {
	check := func(name string, doc *models.FlowDocument) {
		report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
		for _, f := range report.Findings {
			m, ok := LookupMeta(f.RuleID)
			if !ok || f.AutoFix == "" {
				continue
			}
			if f.AutoFix != m.AutoFix && !slices.Contains(m.DirectFixes, f.AutoFix) {
				t.Errorf("%s: %s finding %s carries fix %q its RuleMeta does not declare", name, f.RuleID, f.ID, f.AutoFix)
			}
		}
	}
	for _, in := range findingsGoldenInputs(t) {
		check(in.name, in.build(t))
	}
	direct := buildUncapturedOutputFlow()
	check("uncaptured_output", direct)
	found := false
	for _, f := range RunAnalysis(direct, AllRules(), models.DefaultSettings(), nil).Findings {
		found = found || f.AutoFix == "append-output"
	}
	if !found {
		t.Error("the uncaptured-output probe produced no append-output finding; the direct-fix path is untested")
	}
}

// buildUncapturedOutputFlow calls a subflow that declares an output without
// capturing it: subflow-mismatch stamps its direct fix append-output.
func buildUncapturedOutputFlow() *models.FlowDocument {
	doc := &models.FlowDocument{ID: "uncaptured-output", Subflows: []models.Subflow{
		{ID: "sf1", Name: "Main", Blocks: []models.Block{{ID: "c1", Name: "Call Helper", Type: models.BlockTypeSubflow,
			RawType: "CALL", LineNumber: 1, SubflowID: "sf1", Properties: map[string]string{}}}},
		{ID: "sf2", Name: "Helper", Variables: []models.VariableDecl{{Name: "Output_Result", Type: "string", Scope: "output"}},
			Blocks: []models.Block{{ID: "h1", Name: "Work", Type: models.BlockTypeAction, RawType: "SET", LineNumber: 3,
				SubflowID: "sf2", Properties: map[string]string{"_output": "Output_Result"}}}},
	}}
	doc.RebuildIndexes()
	return doc
}

// TestRuleMeta_KnownCategories keeps Category() inside the set the findings UI
// knows about.
func TestRuleMeta_KnownCategories(t *testing.T) {
	for _, r := range AllRules() {
		if !slices.Contains(knownCategories, r.Category()) {
			t.Errorf("%s: category %q is not one of %v", r.ID(), r.Category(), knownCategories)
		}
	}
}

// TestRuleConfidence_Pinned pins every non-Medium confidence, moved from the
// engine's side table into RuleMeta.
func TestRuleConfidence_Pinned(t *testing.T) {
	want := map[string]models.Confidence{
		"circular-subflow-dependency": models.ConfidenceHigh,
		"empty-branch":                models.ConfidenceHigh,
		"empty-handler":               models.ConfidenceHigh,
		"hardcoded-credential":        models.ConfidenceHigh,
		"infinite-loop-risk":          models.ConfidenceHigh,
		"parse-error":                 models.ConfidenceHigh,
		"redundant-action":            models.ConfidenceHigh,
		"resource-leak":               models.ConfidenceHigh,
		"sensitive-exposure":          models.ConfidenceHigh,
		"sql-injection-risk":          models.ConfidenceHigh,
		"deep-nesting":                models.ConfidenceLow,
		"hardcoded-filepath":          models.ConfidenceLow,
		"hardcoded-ip":                models.ConfidenceLow,
		"hardcoded-url":               models.ConfidenceLow,
		"large-subflow":               models.ConfidenceLow,
		"uninitialized-variable":      models.ConfidenceLow,
		"unused-variable":             models.ConfidenceLow,
		"wide-loop":                   models.ConfidenceLow,
	}
	for _, r := range AllRules() {
		w, ok := want[r.ID()]
		if !ok {
			w = models.ConfidenceMedium
		}
		if got := RuleConfidence(r.ID()); got != w {
			t.Errorf("RuleConfidence(%q) = %q, want %q", r.ID(), got, w)
		}
	}
}

func TestHardcodedCredential_DefaultSeverityIsError(t *testing.T) {
	if got := (&HardcodedCredentialRule{}).DefaultSeverity(); got != models.SeverityError {
		t.Errorf("DefaultSeverity = %q, want error (decision D2)", got)
	}
}

// TestRuleEnabled_AbsentEntryUsesDisabledByDefault: persisted rule maps are
// never rewritten, so a rule missing from them runs by its own default, in the
// engine and in the suppression inventory alike.
func TestRuleEnabled_AbsentEntryUsesDisabledByDefault(t *testing.T) {
	rule := &WaitZeroRule{}
	if !RuleEnabled(rule, nil) {
		t.Fatal("wait-zero must be enabled by default")
	}
	m, _ := LookupMeta("wait-zero")
	m.DisabledByDefault = true
	restore := SetRuleMetaForTesting("wait-zero", m)
	defer restore()

	if RuleEnabled(rule, nil) {
		t.Error("DisabledByDefault rule enabled under nil settings")
	}
	empty := &models.AppSettings{}
	if RuleEnabled(rule, empty) {
		t.Error("DisabledByDefault rule enabled when settings have no entry for it")
	}
	on := &models.AppSettings{Analysis: models.AnalysisSettings{Rules: map[string]models.RuleConfig{"wait-zero": {Enabled: true}}}}
	if !RuleEnabled(rule, on) {
		t.Error("an explicit settings entry must win over DisabledByDefault")
	}
	if def := DefaultRuleConfigs()["wait-zero"]; def.Enabled {
		t.Error("DefaultRuleConfigs must report a DisabledByDefault rule as disabled")
	}

	src := "#Region \"Main\"\n    WAIT 0\n    # pad-ignore[wait-zero]\n    WAIT 0\n#EndRegion\n"
	doc := parseGoldenText(t, src, "Main.txt")
	rules := []Rule{rule}
	if r := RunAnalysis(doc, rules, nil, nil); r.Stats.RulesRun != 0 || len(r.Findings) != 0 {
		t.Errorf("engine ran a DisabledByDefault rule: rulesRun=%d findings=%d", r.Stats.RulesRun, len(r.Findings))
	}
	if r := RunAnalysis(doc, rules, on, nil); r.Stats.RulesRun != 1 || len(r.Findings) == 0 {
		t.Errorf("engine skipped an explicitly enabled rule: rulesRun=%d findings=%d", r.Stats.RulesRun, len(r.Findings))
	}
	inv := SuppressionInventory(doc, rules, nil)
	if len(inv) != 1 || !inv[0].Stale {
		t.Errorf("inventory under nil settings = %+v, want the one directive stale (rule not run)", inv)
	}
	inv = SuppressionInventory(doc, rules, on)
	if len(inv) != 1 || inv[0].Stale {
		t.Errorf("inventory with the rule enabled = %+v, want the one directive live", inv)
	}
}

// TestRuleEnabled_ParseErrorFallbackUsesDisabledByDefault: the document-level
// parse-error fallback (parse-error not in the rule list) follows the same
// default as the engine filter, and the inventory agrees with the engine.
func TestRuleEnabled_ParseErrorFallbackUsesDisabledByDefault(t *testing.T) {
	m, _ := LookupMeta("parse-error")
	m.DisabledByDefault = true
	restore := SetRuleMetaForTesting("parse-error", m)
	defer restore()

	flow := func(directive bool) *models.FlowDocument {
		act := makeBlock("b1", "Click", models.BlockTypeAction, "WebAutomation.Click", 0)
		act.SubflowID, act.LineNumber = "sf2", 3
		blocks := []models.Block{*act}
		if directive {
			cmt := makeBlock("c1", "pad-ignore[parse-error]", models.BlockTypeComment, "COMMENT", 0)
			cmt.SubflowID, cmt.LineNumber = "sf2", 2
			blocks = append([]models.Block{*cmt}, blocks...)
		}
		return goldenIndexed(&models.FlowDocument{
			ID:          "t",
			ParseErrors: []models.ParseError{{Line: 1, Message: "malformed line", Severity: "error"}},
			Subflows:    []models.Subflow{{ID: "sf1", Name: "Main"}, {ID: "sf2", Name: "Helper", Blocks: blocks}},
		})
	}
	parseErrors := func(r *models.AnalysisReport) int {
		n := 0
		for _, f := range r.Findings {
			if f.RuleID == "parse-error" {
				n++
			}
		}
		return n
	}
	rules := []Rule{&WaitZeroRule{}}
	on := &models.AppSettings{Analysis: models.AnalysisSettings{Rules: map[string]models.RuleConfig{"parse-error": {Enabled: true}}}}

	if n := parseErrors(RunAnalysis(flow(false), rules, nil, nil)); n != 0 {
		t.Errorf("fallback under nil settings emitted %d parse-error findings, want 0", n)
	}
	if n := parseErrors(RunAnalysis(flow(false), rules, &models.AppSettings{}, nil)); n != 0 {
		t.Errorf("fallback with no settings entry emitted %d parse-error findings, want 0", n)
	}
	if n := parseErrors(RunAnalysis(flow(false), rules, on, nil)); n != 1 {
		t.Errorf("fallback with parse-error explicitly enabled emitted %d findings, want 1", n)
	}

	inv := SuppressionInventory(flow(true), rules, nil)
	if len(inv) != 1 || !inv[0].Stale {
		t.Errorf("inventory under nil settings = %+v, want the parse-error directive stale", inv)
	}
	inv = SuppressionInventory(flow(true), rules, on)
	if len(inv) != 1 || inv[0].Stale {
		t.Errorf("inventory with parse-error enabled = %+v, want the directive live", inv)
	}
}

// TestStampFindings_PreviewStage: a preview rule's findings carry
// Stage "preview"; a stable rule's carry none, so stable JSON is unchanged.
func TestStampFindings_PreviewStage(t *testing.T) {
	src := "#Region \"Main\"\n    WAIT 0\n    WAIT 0\n#EndRegion\n"
	doc := parseGoldenText(t, src, "Main.txt")
	rules := []Rule{&WaitZeroRule{}}

	stable := RunAnalysis(doc, rules, nil, nil)
	if len(stable.Findings) == 0 {
		t.Fatal("no wait-zero findings")
	}
	for _, f := range stable.Findings {
		if f.Stage != "" {
			t.Errorf("stable finding stamped Stage %q", f.Stage)
		}
		b, _ := json.Marshal(f)
		if strings.Contains(string(b), `"stage"`) {
			t.Errorf("stable finding JSON carries a stage: %s", b)
		}
	}

	m, _ := LookupMeta("wait-zero")
	m.Stage = StagePreview
	restore := SetRuleMetaForTesting("wait-zero", m)
	if !IsPreview("wait-zero") {
		t.Error("IsPreview after the flip = false")
	}
	preview := RunAnalysis(doc, rules, nil, nil)
	for _, f := range preview.Findings {
		if f.Stage != string(StagePreview) {
			t.Errorf("preview finding Stage = %q", f.Stage)
		}
	}
	restore()
	if IsPreview("wait-zero") {
		t.Error("restore did not undo the preview flip")
	}
	if got, _ := LookupMeta("wait-zero"); got.Stage != StageStable {
		t.Errorf("after restore Stage = %q", got.Stage)
	}
}

// TestSetRuleMetaForTesting_UnknownIDAndCopy: a test fake's ID can get
// metadata, and the registry keeps its own copy of the slices it is given.
func TestSetRuleMetaForTesting_UnknownIDAndCopy(t *testing.T) {
	fixes := []string{"remove-block"}
	restore := SetRuleMetaForTesting("fake-rule", RuleMeta{Confidence: models.ConfidenceHigh, DirectFixes: fixes, Stage: StagePreview})
	fixes[0] = "mutated"
	m, ok := LookupMeta("fake-rule")
	if !ok || m.DirectFixes[0] != "remove-block" || !IsPreview("fake-rule") {
		t.Errorf("fake rule meta = %+v, %v", m, ok)
	}
	m.DirectFixes[0] = "mutated-again"
	if again, _ := LookupMeta("fake-rule"); again.DirectFixes[0] != "remove-block" {
		t.Error("LookupMeta handed out the registry's own slice")
	}
	restore()
	if _, ok := LookupMeta("fake-rule"); ok {
		t.Error("restore left the fake rule's metadata behind")
	}
}

// TestMetaOf_CustomRuleAndWrappers: a custom rule's metadata comes from its
// config; a wrapper around a built-in keeps the built-in's by ID; anything
// else is Medium and stable.
func TestMetaOf_CustomRuleAndWrappers(t *testing.T) {
	cr, err := NewCustomRule(CustomRuleConfig{ID: "org-no-wait", Name: "No wait", Severity: "info", TypeMatch: "wait", AutoFix: "remove-block"})
	if err != nil {
		t.Fatal(err)
	}
	m := MetaOf(cr)
	if m.Confidence != models.ConfidenceMedium || m.Stage != StageStable || m.AutoFix != "" ||
		!reflect.DeepEqual(m.DirectFixes, []string{"remove-block"}) {
		t.Errorf("custom rule meta = %+v", m)
	}
	if !mayEmitAutoFix(cr) || !RuleEnabled(cr, nil) {
		t.Error("a custom rule with an autoFix must be fixable and enabled by default")
	}

	type wrapped struct{ Rule }
	if got := MetaOf(wrapped{&ResourceLeakRule{}}); got.AutoFix != "insert-close" || got.Confidence != models.ConfidenceHigh {
		t.Errorf("wrapper meta = %+v, want the built-in's", got)
	}
	other := MetaOf(stubRule{id: "not-a-rule"})
	if other.Confidence != models.ConfidenceMedium || other.Stage != StageStable {
		t.Errorf("unknown rule meta = %+v", other)
	}
}

type stubRule struct{ id string }

func (s stubRule) ID() string                                         { return s.id }
func (s stubRule) Name() string                                       { return s.id }
func (s stubRule) Description() string                                { return "" }
func (s stubRule) DefaultSeverity() models.Severity                   { return models.SeverityInfo }
func (s stubRule) Category() string                                   { return "Style" }
func (s stubRule) Check(*models.Block, *RuleContext) []models.Finding { return nil }
