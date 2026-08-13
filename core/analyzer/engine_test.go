package analyzer

import (
	"context"
	"fmt"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

func makeBlock(id, name string, bt models.BlockType, rawType string, indent int) *models.Block {
	return &models.Block{
		ID:       id,
		Name:     name,
		Type:     bt,
		RawType:  rawType,
		Indent:   indent,
		Children: []models.Block{},
	}
}

// BenchmarkRunAnalysis exercises the single-pass analyzer (one tree traversal
// dispatching all rules per block) on a sizable synthetic flow. Before the
// single-pass refactor this walked the tree once per rule (rules × blocks).
func BenchmarkRunAnalysis(b *testing.B) {
	const n = 2000
	blocks := make([]models.Block, 0, n)
	for i := 0; i < n; i++ {
		blk := makeBlock(
			fmt.Sprintf("b%d", i),
			fmt.Sprintf("Click button %d", i),
			models.BlockTypeAction,
			"WebAutomation.Click.Click",
			0,
		)
		blk.SubflowID = "sf1"
		blocks = append(blocks, *blk)
	}
	flow := &models.FlowDocument{
		ID:       "bench",
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: blocks}},
	}
	rules := AllRules()

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = RunAnalysis(flow, rules, nil, nil)
	}
}

func TestRunAnalysisEmpty(t *testing.T) {
	flow := &models.FlowDocument{
		ID:       "test",
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{}}},
	}
	report := RunAnalysis(flow, AllRules(), nil, nil)
	if len(report.Findings) != 0 {
		t.Errorf("expected 0 findings, got %d", len(report.Findings))
	}
	if report.Stats.BlocksAnalyzed != 0 {
		t.Errorf("expected 0 blocks analyzed, got %d", report.Stats.BlocksAnalyzed)
	}
}

func TestRunAnalysisPopulatesFingerprint(t *testing.T) {
	b := makeBlock("b1", "HTTP Request", models.BlockTypeAction, "Web.Call", 0)
	b.SubflowID = "sf1"
	b.Properties = map[string]string{"url": "https://api.example.com/v2/users"}
	flow := &models.FlowDocument{
		ID:       "test",
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}},
	}

	report := RunAnalysis(flow, AllRules(), nil, nil)
	if len(report.Findings) == 0 {
		t.Fatal("expected at least one finding to assert fingerprint on")
	}
	for _, f := range report.Findings {
		if f.Fingerprint == "" {
			t.Errorf("finding %s has empty Fingerprint", f.ID)
		}
		if f.Fingerprint != f.Key() {
			t.Errorf("Fingerprint %q != Key() %q", f.Fingerprint, f.Key())
		}
	}
}

func TestRunAnalysisProgress(t *testing.T) {
	b := makeBlock("b1", "Click button", models.BlockTypeAction, "WebAutomation.Click", 0)
	b.SubflowID = "sf1"
	flow := &models.FlowDocument{
		ID:       "test",
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}},
	}
	calls := 0
	RunAnalysis(flow, AllRules()[:1], nil, func(current, total int, ruleName string) {
		calls++
	})
	if calls == 0 {
		t.Error("expected progress callbacks")
	}
}

func TestBuildVariableLineage(t *testing.T) {
	t.Run("variable with set and read", func(t *testing.T) {
		setter := makeBlock("b1", "Set X", models.BlockTypeVariable, "SET", 0)
		setter.Properties = map[string]string{"_output": "Counter"}
		setter.SubflowID = "sf1"

		user := makeBlock("b2", "Use X", models.BlockTypeAction, "Display.ShowMessageBox", 0)
		user.Variables = []string{"Counter"}
		user.SubflowID = "sf1"

		doc := &models.FlowDocument{
			ID:       "test",
			Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*setter, *user}}},
		}

		h := BuildVariableLineage(doc, "Counter")
		if h == nil {
			t.Fatal("expected non-nil history")
		}
		if h.Name != "Counter" {
			t.Errorf("Name = %q, want %q", h.Name, "Counter")
		}
		initCount, readCount := 0, 0
		for _, e := range h.Events {
			switch e.Type {
			case "init":
				initCount++
			case "read":
				readCount++
			}
		}
		if initCount != 1 {
			t.Errorf("expected 1 init event, got %d", initCount)
		}
		if readCount != 1 {
			t.Errorf("expected 1 read event, got %d", readCount)
		}
	})

	t.Run("unknown variable returns empty events", func(t *testing.T) {
		doc := &models.FlowDocument{
			ID:       "test",
			Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{}}},
		}
		h := BuildVariableLineage(doc, "NoSuchVar")
		if h == nil {
			t.Fatal("expected non-nil history for unknown var")
		}
		if len(h.Events) != 0 {
			t.Errorf("expected 0 events, got %d", len(h.Events))
		}
	})
}

func TestBuildExecutionGraph(t *testing.T) {
	t.Run("single subflow has one node and no edges", func(t *testing.T) {
		doc := &models.FlowDocument{
			ID:       "test",
			Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{}}},
		}
		g := BuildExecutionGraph(doc, nil)
		if g == nil {
			t.Fatal("expected non-nil graph")
		}
		if len(g.Nodes) != 1 {
			t.Errorf("expected 1 node, got %d", len(g.Nodes))
		}
		if len(g.Edges) != 0 {
			t.Errorf("expected 0 edges, got %d", len(g.Edges))
		}
	})

	t.Run("two subflows with call have edge", func(t *testing.T) {
		callBlock := makeBlock("b1", "Call Helper", models.BlockTypeAction, "CALL", 0)
		callBlock.SubflowID = "sf1"
		doc := &models.FlowDocument{
			ID: "test",
			Subflows: []models.Subflow{
				{ID: "sf1", Name: "Main", Blocks: []models.Block{*callBlock}},
				{ID: "sf2", Name: "Helper", Blocks: []models.Block{}},
			},
		}
		g := BuildExecutionGraph(doc, nil)
		if len(g.Nodes) != 2 {
			t.Errorf("expected 2 nodes, got %d", len(g.Nodes))
		}
		if len(g.Edges) != 1 {
			t.Errorf("expected 1 edge (Main→Helper), got %d", len(g.Edges))
		}
		if g.Edges[0].Source != "sf1" || g.Edges[0].Target != "sf2" {
			t.Errorf("edge = %v→%v, want sf1→sf2", g.Edges[0].Source, g.Edges[0].Target)
		}
	})

	t.Run("duplicate calls produce single edge", func(t *testing.T) {
		c1 := makeBlock("b1", "Call Helper", models.BlockTypeAction, "CALL", 0)
		c1.SubflowID = "sf1"
		c2 := makeBlock("b2", "Call Helper", models.BlockTypeAction, "CALL", 0)
		c2.SubflowID = "sf1"
		doc := &models.FlowDocument{
			ID: "test",
			Subflows: []models.Subflow{
				{ID: "sf1", Name: "Main", Blocks: []models.Block{*c1, *c2}},
				{ID: "sf2", Name: "Helper", Blocks: []models.Block{}},
			},
		}
		g := BuildExecutionGraph(doc, nil)
		if len(g.Edges) != 1 {
			t.Errorf("expected 1 deduplicated edge, got %d", len(g.Edges))
		}
	})

	t.Run("findings annotate node counts", func(t *testing.T) {
		doc := &models.FlowDocument{
			ID:       "test",
			Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{}}},
		}
		report := &models.AnalysisReport{
			Findings: []models.Finding{
				{SubflowID: "sf1", Severity: models.SeverityError},
				{SubflowID: "sf1", Severity: models.SeverityWarning},
			},
		}
		g := BuildExecutionGraph(doc, report)
		if g.Nodes[0].ErrorCount != 1 {
			t.Errorf("ErrorCount = %d, want 1", g.Nodes[0].ErrorCount)
		}
		if g.Nodes[0].WarnCount != 1 {
			t.Errorf("WarnCount = %d, want 1", g.Nodes[0].WarnCount)
		}
	})
}

func TestComputeStats(t *testing.T) {
	findings := []models.Finding{
		{Severity: models.SeverityError},
		{Severity: models.SeverityWarning},
		{Severity: models.SeverityWarning},
		{Severity: models.SeverityInfo},
	}
	stats := computeStats(findings)
	if stats.Errors != 1 || stats.Warnings != 2 || stats.Info != 1 {
		t.Errorf("stats mismatch: %+v", stats)
	}
}

// panickingRule is a synthetic Rule whose Check always panics. Used to prove
// safeCheck's recovery surfaces on AnalysisStats.RulesSkipped (M11) instead of
// aborting the whole run silently.
type panickingRule struct{}

func (panickingRule) ID() string                       { return "panicking-test-rule" }
func (panickingRule) Name() string                     { return "Panicking test rule" }
func (panickingRule) Description() string              { return "Always panics; for RulesSkipped surfacing" }
func (panickingRule) DefaultSeverity() models.Severity { return models.SeverityWarning }
func (panickingRule) Category() string                 { return "test" }
func (panickingRule) Check(_ *models.Block, _ *RuleContext) []models.Finding {
	panic("intentional test panic")
}

// safeRule is a non-panicking rule that emits one finding per call, used to
// prove the rest of the run still completes after a panic in another rule.
type safeRule struct{}

func (safeRule) ID() string                       { return "safe-test-rule" }
func (safeRule) Name() string                     { return "Safe test rule" }
func (safeRule) Description() string              { return "Emits one finding; for RulesSkipped surfacing" }
func (safeRule) DefaultSeverity() models.Severity { return models.SeverityWarning }
func (safeRule) Category() string                 { return "test" }
func (safeRule) Check(block *models.Block, _ *RuleContext) []models.Finding {
	return []models.Finding{{RuleID: "safe-test-rule", BlockID: block.ID, Severity: models.SeverityWarning}}
}

// TestRunAnalysis_RulesSkippedSurfaced proves safeCheck's panic recovery is
// observable on the report: when a rule panics, RulesSkipped > 0 AND the run
// still completes for other (non-panicking) rules. Previously the count was
// only logged, so operators couldn't tell from SARIF/JSON that findings may be
// missing.
func TestRunAnalysis_RulesSkippedSurfaced(t *testing.T) {
	flow := &models.FlowDocument{
		Name: "panic-test",
		Subflows: []models.Subflow{{
			ID:     "sf1",
			Name:   "Main",
			Blocks: []models.Block{*makeBlock("b1", "x", models.BlockTypeAction, "Display.UiFlow", 0)},
		}},
	}
	flow.RebuildIndexes()

	rules := []Rule{panickingRule{}, safeRule{}}
	report := RunAnalysis(flow, rules, nil, nil)

	if report.Stats.RulesSkipped == 0 {
		t.Errorf("expected RulesSkipped > 0 when a rule panics, got %d", report.Stats.RulesSkipped)
	}
	// The safe rule's findings must still appear — the panic in the other rule
	// did not abort the run.
	if len(report.Findings) == 0 {
		t.Errorf("expected at least one finding from the non-panicking rule, got 0")
	}
}

// TestRunAnalysisCtx_RespectsCancellation verifies the walk honours a cancelled
// context: rule work is skipped for remaining blocks, bounding the CPU a single
// pathological payload can burn on the raw-analyze endpoint (whose per-request
// deadline cancels the context).
func TestRunAnalysisCtx_RespectsCancellation(t *testing.T) {
	src := "#Region \"Main\"\nSET X TO %X%\n#EndRegion\n"
	doc, err := parser.ParseText(src, "Main.txt", int64(len(src)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	baseline := RunAnalysis(doc, AllRules(), nil, nil)
	if len(baseline.Findings) == 0 {
		t.Fatal("baseline: expected findings, got 0")
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	cancelled := RunAnalysisCtx(ctx, doc, AllRules(), nil, nil)
	if len(cancelled.Findings) != 0 {
		t.Errorf("expected 0 findings with a pre-cancelled ctx (rule work skipped), got %d", len(cancelled.Findings))
	}
}
