package analyzer

import (
	"context"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// findingsAt is a flow rule's findings anchored on block: the unit tests'
// stand-in for the per-block Check the rule had before it became a FlowRule.
func findingsAt(r FlowRule, ctx *RuleContext, block *models.Block) []models.Finding {
	var out []models.Finding
	for _, f := range r.CheckFlow(ctx) {
		if f.BlockID == block.ID {
			out = append(out, f)
		}
	}
	return out
}

// pinnedFlowRules are the built-ins that check the whole document at once.
var pinnedFlowRules = []string{
	"circular-subflow-dependency",
	"duplicate-subflow-name",
	"empty-subflow",
	"high-cyclomatic-complexity",
	"large-subflow",
	"parse-error",
	"subflow-no-error-handler",
	"uncalled-subflow",
}

func TestAllRules_HaveAKind(t *testing.T) {
	var flow []string
	for _, r := range AllRules() {
		switch kindOf(r) {
		case kindFlow:
			flow = append(flow, r.ID())
		case kindBlock:
		default:
			t.Errorf("%s (%T) is neither a BlockRule nor a FlowRule: the engine would skip it", r.ID(), r)
		}
	}
	slices.Sort(flow)
	if !slices.Equal(flow, pinnedFlowRules) {
		t.Errorf("flow rules = %v, want %v", flow, pinnedFlowRules)
	}
}

// identityRule is a Rule of no kind: what a wrapper that embeds only the
// identity looks like to the engine.
type identityRule struct{ id string }

func (r identityRule) ID() string                     { return r.id }
func (r identityRule) Name() string                   { return "Identity " + r.id }
func (identityRule) Description() string              { return "" }
func (identityRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (identityRule) Category() string                 { return "test" }

// testFlowRule returns canned findings (or panics) from CheckFlow.
type testFlowRule struct {
	identityRule
	findings func(ctx *RuleContext) []models.Finding
	calls    *int
}

func (r testFlowRule) CheckFlow(ctx *RuleContext) []models.Finding {
	if r.calls != nil {
		*r.calls++
	}
	return r.findings(ctx)
}

func threeBlockFlow() *models.FlowDocument {
	src := "#Region \"Main\"\n    SET A TO 1\n    SET B TO 2\n    SET C TO 3\n#EndRegion\n"
	doc, err := parser.ParseText(src, "Main.txt", int64(len(src)))
	if err != nil {
		panic(err)
	}
	return doc
}

func TestEngine_FlowRuleFindingsInDocumentOrderInTheirSlot(t *testing.T) {
	doc := threeBlockFlow()
	blocks := doc.Subflows[0].Blocks
	flowRule := testFlowRule{identityRule: identityRule{"test-flow"}, findings: func(*RuleContext) []models.Finding {
		return []models.Finding{
			{RuleID: "test-flow", Title: "blockless", SubflowID: doc.Subflows[0].ID},
			{RuleID: "test-flow", Title: "third", BlockID: blocks[2].ID},
			{RuleID: "test-flow", Title: "first", BlockID: blocks[0].ID},
			{RuleID: "test-flow", Title: "second-a", BlockID: blocks[1].ID},
			{RuleID: "test-flow", Title: "second-b", BlockID: blocks[1].ID},
		}
	}}
	report := RunAnalysis(doc, []Rule{safeRule{}, flowRule, safeRule{}}, nil, nil)

	var titles []string
	for _, f := range report.Findings {
		if f.RuleID == "test-flow" {
			titles = append(titles, f.Title)
		}
	}
	if want := []string{"first", "second-a", "second-b", "third", "blockless"}; !slices.Equal(titles, want) {
		t.Fatalf("flow rule findings = %v, want %v (document order, stable, block-less last)", titles, want)
	}
	// Rule-major order: safe-test-rule x3 blocks, then the flow rule, then
	// safe-test-rule again (dedup drops the second safe rule's findings).
	if got := report.Findings[3].RuleID; got != "test-flow" {
		t.Errorf("finding 4 is %s, want the flow rule's first", got)
	}
	if report.Findings[3].Category != "test" {
		t.Errorf("flow finding category = %q, want the rule's category", report.Findings[3].Category)
	}
	for _, p := range report.RuleProfiles {
		if p.BlocksChecked != report.Stats.BlocksAnalyzed {
			t.Errorf("%s: BlocksChecked %d, want %d", p.RuleID, p.BlocksChecked, report.Stats.BlocksAnalyzed)
		}
	}
}

func TestEngine_FlowRulePanicIsSkipped(t *testing.T) {
	doc := threeBlockFlow()
	boom := testFlowRule{identityRule: identityRule{"test-boom"}, findings: func(*RuleContext) []models.Finding { panic("boom") }}
	var events, lastCur, lastTotal int
	report := RunAnalysis(doc, []Rule{boom, safeRule{}}, nil, func(cur, total int, _ string) {
		events++
		lastCur, lastTotal = cur, total
	})
	if report.Stats.RulesSkipped != 1 {
		t.Errorf("RulesSkipped = %d, want 1", report.Stats.RulesSkipped)
	}
	if len(report.Findings) != 3 {
		t.Errorf("findings = %d, want the safe rule's 3", len(report.Findings))
	}
	if lastCur != lastTotal || lastTotal != 2*report.Stats.BlocksAnalyzed {
		t.Errorf("final progress %d/%d, want %d/%d", lastCur, lastTotal, 2*report.Stats.BlocksAnalyzed, 2*report.Stats.BlocksAnalyzed)
	}
}

func TestEngine_CancelledBeforeFlowRulesIsIncomplete(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	calls := 0
	rule := testFlowRule{identityRule: identityRule{"test-flow"}, calls: &calls, findings: func(*RuleContext) []models.Finding { return nil }}
	_, complete := runAnalysisWalk(ctx, threeBlockFlow(), []Rule{rule}, nil, nil, false)
	if complete || calls != 0 {
		t.Fatalf("complete = %v, flow rule calls = %d; want an incomplete walk that ran no flow rule", complete, calls)
	}
}

func TestEngine_UnknownKindIsSkippedAndCounted(t *testing.T) {
	doc := threeBlockFlow()
	var lastCur, lastTotal int
	report := RunAnalysis(doc, []Rule{identityRule{"test-no-kind"}, safeRule{}}, nil, func(cur, total int, _ string) {
		lastCur, lastTotal = cur, total
	})
	if report.Stats.RulesSkipped != 1 || report.Stats.RulesRun != 2 {
		t.Errorf("RulesSkipped %d, RulesRun %d; want 1 and 2", report.Stats.RulesSkipped, report.Stats.RulesRun)
	}
	if len(report.Findings) != 3 {
		t.Errorf("findings = %d, want the safe rule's 3", len(report.Findings))
	}
	if lastCur != lastTotal {
		t.Errorf("final progress %d/%d: the skipped rule's checks must still complete the bar", lastCur, lastTotal)
	}
	if _, logged := unknownKindLogged.Load("test-no-kind"); !logged {
		t.Error("the unknown kind was not logged")
	}
}

// TestSeverityOverride_OnlyWhenDifferentFromDefault: a configured
// severity equal to the rule's default leaves the severities the rule grades
// itself alone; a different one overrides every finding of the rule, the
// engine's block-less parse-error fallback included.
func TestSeverityOverride_OnlyWhenDifferentFromDefault(t *testing.T) {
	anchored := threeBlockFlow()
	anchored.ParseErrors = []models.ParseError{{Line: 1, Message: "e", Severity: "error"}, {Line: 2, Message: "w", Severity: "warning"}}
	fallback := threeBlockFlow()
	fallback.ParseErrors = anchored.ParseErrors
	fallback.Subflows[0].Blocks = nil // no anchor: the engine reports them block-less

	sevs := func(doc *models.FlowDocument, s *models.AppSettings) []models.Severity {
		var out []models.Severity
		for _, f := range RunAnalysis(doc, []Rule{&ParseErrorRule{}}, s, nil).Findings {
			out = append(out, f.Severity)
		}
		return out
	}
	withSeverity := func(sev string) *models.AppSettings {
		s := models.DefaultSettings()
		rc := s.Analysis.Rules["parse-error"]
		rc.Severity = sev
		s.Analysis.Rules["parse-error"] = rc
		return s
	}
	graded := []models.Severity{models.SeverityError, models.SeverityWarning}
	for name, doc := range map[string]*models.FlowDocument{"anchored": anchored, "fallback": fallback} {
		if got := sevs(doc, nil); !slices.Equal(got, graded) {
			t.Errorf("%s, nil settings: %v, want %v", name, got, graded)
		}
		if got := sevs(doc, withSeverity("error")); !slices.Equal(got, graded) {
			t.Errorf("%s, configured default: %v, want the parser's grading %v", name, got, graded)
		}
		if got := sevs(doc, withSeverity("info")); !slices.Equal(got, []models.Severity{models.SeverityInfo, models.SeverityInfo}) {
			t.Errorf("%s, configured info: %v, want every finding info", name, got)
		}
	}

	// sql-injection-risk's taint escalation survives DefaultSettings.
	sql := sqlTaintQuery("q", "SELECT * FROM t WHERE name = '%X%'", 1)
	tainted := sqlTaintFlow(sqlTaintHTTP("h", "X", 0), sql).Flow
	for _, s := range []*models.AppSettings{nil, models.DefaultSettings()} {
		var got []models.Severity
		for _, f := range RunAnalysis(tainted, []Rule{&SqlInjectionRiskRule{}}, s, nil).Findings {
			got = append(got, f.Severity)
		}
		if !slices.Equal(got, []models.Severity{models.SeverityError}) {
			t.Errorf("sql-injection-risk (settings %v): %v, want the taint escalation to error", s != nil, got)
		}
	}
}

// TestParseErrorFallback_CarriesTheRuleCategory: the engine's block-less parse-error findings are parse-error's,
// so they carry its category like the anchored ones the walk stamps (the Findings tab, exports and the category
// filters group by it); also when parse-error is not in the run's rule list, and on the truncation summary.
func TestParseErrorFallback_CarriesTheRuleCategory(t *testing.T) {
	want := (&ParseErrorRule{}).Category()
	anchored := threeBlockFlow()
	anchored.ParseErrors = []models.ParseError{{Line: 1, Message: "e", Severity: "error"}}
	fallback := threeBlockFlow()
	fallback.Subflows[0].Blocks = nil
	for i := 0; i <= maxParseErrorFindings; i++ {
		fallback.ParseErrors = append(fallback.ParseErrors, models.ParseError{Line: i + 1, Message: "e", Severity: "warning"})
	}
	for _, tc := range []struct {
		name  string
		doc   *models.FlowDocument
		rules []Rule
		n     int
	}{
		{"anchored", anchored, []Rule{&ParseErrorRule{}}, 1},
		{"block-less", fallback, []Rule{&ParseErrorRule{}}, maxParseErrorFindings + 1},
		{"block-less, parse-error not run", fallback, nil, maxParseErrorFindings + 1},
	} {
		var n, wrong int
		for _, f := range RunAnalysis(tc.doc, tc.rules, nil, nil).Findings {
			if f.RuleID != "parse-error" {
				continue
			}
			n++
			if f.Category != want {
				if wrong++; wrong == 1 {
					t.Errorf("%s: %q (block %q) has category %q, want %q", tc.name, f.Title, f.BlockID, f.Category, want)
				}
			}
		}
		if n != tc.n {
			t.Errorf("%s: %d parse-error findings, want %d", tc.name, n, tc.n)
		}
	}
}

// TestDefaultSettings_MatchNilSettings is the CLI-vs-server parity: bakicli
// without a config passes nil settings, the server the seeded defaults; over
// the whole corpus the two must produce the same report.
func TestDefaultSettings_MatchNilSettings(t *testing.T) {
	for _, in := range findingsGoldenInputs(t) {
		doc := in.build(t)
		a := RunAnalysisCtxNoProfile(context.Background(), doc, AllRules(), nil, nil)
		b := RunAnalysisCtxNoProfile(context.Background(), doc, AllRules(), models.DefaultSettings(), nil)
		a.GeneratedAt, a.DurationMs = b.GeneratedAt, b.DurationMs
		if !reflect.DeepEqual(a, b) {
			for i := range a.Findings {
				if i < len(b.Findings) && !reflect.DeepEqual(a.Findings[i], b.Findings[i]) {
					t.Errorf("%s: finding %d differs:\n nil:      %+v\n defaults: %+v", in.name, i, a.Findings[i], b.Findings[i])
					break
				}
			}
			t.Errorf("%s: nil settings and DefaultSettings give different reports", in.name)
		}
	}
}

// TestSuppressionInventory_RecordsTheFindingsBlock: the inventory used its own
// dispatcher and recorded a finding on the block it dispatched, so the
// empty-branch directive (finding on the ELSE, rule dispatched on the IF) was
// reported stale while analysis suppressed the finding. It now shares the
// engine's dispatcher and records f.BlockID.
func TestSuppressionInventory_RecordsTheFindingsBlock(t *testing.T) {
	src := "#Region \"Main\"\n    IF %A% = 1 THEN\n        SET B TO 2\n        # pad-ignore[empty-branch]\n    ELSE\n    END\n#EndRegion\n"
	doc, err := parser.ParseText(src, "Main.txt", int64(len(src)))
	if err != nil {
		t.Fatal(err)
	}
	if report := RunAnalysis(doc, AllRules(), nil, nil); report.Stats.Suppressed != 1 {
		t.Fatalf("premise: analysis suppressed %d findings, want the empty-branch one", report.Stats.Suppressed)
	}
	inv := SuppressionInventory(doc, AllRules(), nil)
	if len(inv) != 1 || inv[0].Rule != "empty-branch" || inv[0].Stale {
		t.Fatalf("inventory = %+v, want one live empty-branch directive", inv)
	}

	// And a panicking rule no longer takes the inventory down.
	boom := testFlowRule{identityRule: identityRule{"test-boom"}, findings: func(*RuleContext) []models.Finding { panic("boom") }}
	if inv := SuppressionInventory(doc, append(AllRules(), boom, panickingRule{}), nil); len(inv) != 1 || inv[0].Stale {
		t.Fatalf("inventory with panicking rules = %+v", inv)
	}
}

// TestSuppressionInventory_DoesNotOrderFindings: the inventory needs only
// which rules fire where, so it takes the findings unordered. Ordering a flow
// rule's reversed, nested anchors needs the docPosition map; a later flow rule
// sees whether the walk built it.
func TestSuppressionInventory_DoesNotOrderFindings(t *testing.T) {
	src := "#Region \"Main\"\n    # pad-ignore[test-reversed]\n    IF %A% = 1 THEN\n        SET B TO 2\n        SET C TO 3\n    END\n#EndRegion\n"
	doc, err := parser.ParseText(src, "Main.txt", int64(len(src)))
	if err != nil {
		t.Fatal(err)
	}
	nested := doc.Subflows[0].Blocks[1].Children
	reversed := testFlowRule{identityRule: identityRule{"test-reversed"}, findings: func(*RuleContext) []models.Finding {
		return []models.Finding{
			{RuleID: "test-reversed", BlockID: nested[1].ID},
			{RuleID: "test-reversed", BlockID: nested[0].ID},
		}
	}}
	var docPosBuilt bool
	probe := testFlowRule{identityRule: identityRule{"test-probe"}, findings: func(ctx *RuleContext) []models.Finding {
		docPosBuilt = ctx.docPos.built
		return nil
	}}
	rules := []Rule{reversed, probe}

	if RunAnalysis(doc, rules, nil, nil); !docPosBuilt {
		t.Fatal("premise: the analysis did not need the docPosition map")
	}
	docPosBuilt = false
	if inv := SuppressionInventory(doc, rules, nil); len(inv) != 1 || !inv[0].Stale {
		t.Fatalf("inventory = %+v, want the directive stale (the rule fires on the IF's children)", inv)
	}
	if docPosBuilt {
		t.Error("the inventory ordered the flow rule's findings (built the docPosition map)")
	}
}

// TestCollectFindings_VisitSeesTheCollectedFindings: a visiting walk hands
// over exactly the findings (and counts) a collecting walk returns, sharded
// documents included, without the flat list or the docPosition map.
func TestCollectFindings_VisitSeesTheCollectedFindings(t *testing.T) {
	keyOf := func(f *models.Finding) string {
		return fmt.Sprintf("%s|%s|%s|%s|%s|%s|%s|%v", f.RuleID, f.BlockID, f.SubflowID, f.Severity, f.Category, f.Title, f.Description, f.Metadata)
	}
	reversed := testFlowRule{identityRule: identityRule{"test-reversed"}, findings: func(ctx *RuleContext) []models.Finding {
		var out []models.Finding
		for id := range ctx.ParentMap {
			out = append(out, models.Finding{RuleID: "test-reversed", BlockID: id})
		}
		return out
	}}
	rules := append(AllRules(), reversed)
	sharded := false
	for _, d := range oracleDocs(t, 60) {
		for si, settings := range oracleSettings() {
			var enabled []Rule
			for _, r := range rules {
				if RuleEnabled(r, settings) {
					enabled = append(enabled, r)
				}
			}
			want := collectFindings(context.Background(), buildContext(d.doc, settings), enabled, walkOptions{})
			ctx := buildContext(d.doc, settings)
			var got []string
			res := collectFindings(context.Background(), ctx, enabled, walkOptions{visit: func(f *models.Finding) {
				got = append(got, keyOf(f))
			}})
			var wantKeys []string
			for i := range want.findings {
				wantKeys = append(wantKeys, keyOf(&want.findings[i]))
			}
			slices.Sort(got)
			slices.Sort(wantKeys)
			if !slices.Equal(got, wantKeys) || !slices.Equal(res.counts, want.counts) {
				t.Errorf("%s settings#%d: visited %d findings (counts %v), collected %d (counts %v)",
					d.name, si, len(got), res.counts, len(wantKeys), want.counts)
			}
			if res.findings != nil || ctx.docPos.built {
				t.Errorf("%s settings#%d: a visiting walk collected findings or built the docPosition map", d.name, si)
			}
			sharded = sharded || ctx.totalBlocks >= 256
		}
	}
	if !sharded {
		t.Fatal("no document large enough to shard")
	}
}
