package analyzer

import (
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// ── Hardcoded IP ───────────────────────────────────────────────────

func TestHardcodedIPRule(t *testing.T) {
	rule := &HardcodedIPRule{}

	t.Run("IPv4 in server property emits finding", func(t *testing.T) {
		b := makeBlock("b1", "DB Connect", models.BlockTypeAction, "DB.Connect", 0)
		b.SubflowID = "sf1"
		b.Properties = map[string]string{"server": "192.168.1.100"}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
		if got[0].RuleID != "hardcoded-ip" {
			t.Errorf("ruleID = %q", got[0].RuleID)
		}
	})

	t.Run("localhost is skipped", func(t *testing.T) {
		b := makeBlock("b1", "DB Connect", models.BlockTypeAction, "DB.Connect", 0)
		b.SubflowID = "sf1"
		b.Properties = map[string]string{"server": "127.0.0.1"}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings for localhost, got %d", len(got))
		}
	})

	t.Run("variable reference is skipped", func(t *testing.T) {
		b := makeBlock("b1", "DB Connect", models.BlockTypeAction, "DB.Connect", 0)
		b.SubflowID = "sf1"
		b.Properties = map[string]string{"server": "%ServerIP%"}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings for variable reference, got %d", len(got))
		}
	})

	t.Run("non-network property is skipped", func(t *testing.T) {
		b := makeBlock("b1", "Set", models.BlockTypeVariable, "SET", 0)
		b.SubflowID = "sf1"
		b.Properties = map[string]string{"value": "192.168.1.100"}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings for non-network property, got %d", len(got))
		}
	})
}

// TestHardcodedIPRule_RoundTripResolvesFinding verifies the replace-with-variable
// fixer resolves a hardcoded IP finding.
func TestHardcodedIPRule_RoundTripResolvesFinding(t *testing.T) {
	const source = "#Region \"Main\"\nDB.Connect Server: 192.168.1.100\n#EndRegion\n"
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	report := RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil)
	var ipFinding *models.Finding
	for i := range report.Findings {
		if report.Findings[i].RuleID == "hardcoded-ip" {
			ipFinding = &report.Findings[i]
			break
		}
	}
	if ipFinding == nil {
		t.Fatalf("expected hardcoded-ip finding, got: %v", ruleIDs(report.Findings))
	}
	block := doc.BlocksByID[ipFinding.BlockID]
	if block == nil {
		t.Fatalf("block not found")
	}
	propKey, _ := ipFinding.Metadata["property"].(string)

	patched := ApplyPatch(source, ReplaceWithVariablePatch(block, propKey))
	doc2, err := parser.ParseText(patched, "Main.txt", int64(len(patched)))
	if err != nil {
		t.Fatalf("re-parse failed: %v\npatched:\n%s", err, patched)
	}
	report2 := RunAnalysis(doc2, AllRules(), models.DefaultSettings(), nil)
	for _, f := range report2.Findings {
		if f.RuleID == "hardcoded-ip" {
			t.Errorf("hardcoded-ip still present after fix\npatched:\n%s", patched)
		}
	}
}

// ── Circular Subflow Dependency ────────────────────────────────────

func TestCircularSubflowRule(t *testing.T) {
	t.Run("mutual calls emit findings", func(t *testing.T) {
		// A → B → A (cycle)
		callB := makeBlock("a_call", "Call B", models.BlockTypeSubflow, "CALL", 0)
		callA := makeBlock("b_call", "Call A", models.BlockTypeSubflow, "CALL", 0)
		sfA := makeSubflow("sfa", "A", callB)
		sfB := makeSubflow("sfb", "B", callA)
		flow := makeFlowWithSubflows(sfA, sfB)

		ctx := buildContext(flow, nil)
		rule := &CircularSubflowRule{}

		// Fire on first block of subflow A
		got := rule.Check(callB, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding for cyclic subflow A, got %d", len(got))
		}
		if got[0].RuleID != "circular-subflow-dependency" {
			t.Errorf("ruleID = %q", got[0].RuleID)
		}

		// Fire on first block of subflow B
		gotB := rule.Check(callA, ctx)
		if len(gotB) != 1 {
			t.Fatalf("expected 1 finding for cyclic subflow B, got %d", len(gotB))
		}
	})

	t.Run("no cycle emits no finding", func(t *testing.T) {
		// A → B → C (no cycle)
		callB := makeBlock("a_call", "Call B", models.BlockTypeSubflow, "CALL", 0)
		callC := makeBlock("b_call", "Call C", models.BlockTypeSubflow, "CALL", 0)
		action := makeBlock("c_act", "work", models.BlockTypeAction, "SET", 0)
		sfA := makeSubflow("sfa", "A", callB)
		sfB := makeSubflow("sfb", "B", callC)
		sfC := makeSubflow("sfc", "C", action)
		flow := makeFlowWithSubflows(sfA, sfB, sfC)

		ctx := buildContext(flow, nil)
		rule := &CircularSubflowRule{}
		got := rule.Check(callB, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings for acyclic graph, got %d", len(got))
		}
	})

	t.Run("non-first block emits no finding", func(t *testing.T) {
		// A → B → A (cycle), but check fires on second block of A
		callB := makeBlock("a_call", "Call B", models.BlockTypeSubflow, "CALL", 0)
		extra := makeBlock("a_extra", "extra", models.BlockTypeAction, "SET", 0)
		callA := makeBlock("b_call", "Call A", models.BlockTypeSubflow, "CALL", 0)
		sfA := makeSubflow("sfa", "A", callB, extra)
		sfB := makeSubflow("sfb", "B", callA)
		flow := makeFlowWithSubflows(sfA, sfB)

		ctx := buildContext(flow, nil)
		rule := &CircularSubflowRule{}
		got := rule.Check(extra, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings on non-first block, got %d", len(got))
		}
	})
}

// ── Parse Error ────────────────────────────────────────────────────

func TestParseErrorRule(t *testing.T) {
	t.Run("parse errors emitted as findings", func(t *testing.T) {
		block := makeBlock("b1", "Action", models.BlockTypeAction, "SET", 0)
		block.SubflowID = "sf1"
		flow := &models.FlowDocument{
			ID:       "test",
			Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*block}}},
			ParseErrors: []models.ParseError{
				{Line: 5, Message: "unclosed block: IF", Severity: "error"},
				{Line: 10, Message: "malformed line", Severity: "warning"},
			},
		}
		ctx := buildContext(flow, nil)
		rule := &ParseErrorRule{}
		got := rule.Check(block, ctx)
		if len(got) != 2 {
			t.Fatalf("expected 2 findings, got %d", len(got))
		}
		if got[0].RuleID != "parse-error" {
			t.Errorf("ruleID = %q", got[0].RuleID)
		}
		if got[0].Description != "unclosed block: IF" {
			t.Errorf("description = %q", got[0].Description)
		}
	})

	t.Run("no parse errors emits nothing", func(t *testing.T) {
		block := makeBlock("b1", "Action", models.BlockTypeAction, "SET", 0)
		block.SubflowID = "sf1"
		flow := &models.FlowDocument{
			ID:          "test",
			Subflows:    []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*block}}},
			ParseErrors: nil,
		}
		ctx := buildContext(flow, nil)
		rule := &ParseErrorRule{}
		got := rule.Check(block, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("non-first block emits nothing", func(t *testing.T) {
		first := makeBlock("b1", "First", models.BlockTypeAction, "SET", 0)
		first.SubflowID = "sf1"
		second := makeBlock("b2", "Second", models.BlockTypeAction, "SET", 0)
		second.SubflowID = "sf1"
		flow := &models.FlowDocument{
			ID:       "test",
			Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*first, *second}}},
			ParseErrors: []models.ParseError{
				{Line: 1, Message: "error", Severity: "error"},
			},
		}
		ctx := buildContext(flow, nil)
		rule := &ParseErrorRule{}
		got := rule.Check(second, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings on non-first block, got %d", len(got))
		}
	})

	t.Run("severity mapping", func(t *testing.T) {
		block := makeBlock("b1", "Action", models.BlockTypeAction, "SET", 0)
		block.SubflowID = "sf1"
		flow := &models.FlowDocument{
			ID:       "test",
			Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*block}}},
			ParseErrors: []models.ParseError{
				{Line: 1, Message: "err", Severity: "error"},
				{Line: 2, Message: "warn", Severity: "warning"},
				{Line: 3, Message: "info", Severity: "info"},
			},
		}
		ctx := buildContext(flow, nil)
		rule := &ParseErrorRule{}
		got := rule.Check(block, ctx)
		if len(got) != 3 {
			t.Fatalf("expected 3 findings, got %d", len(got))
		}
		if got[0].Severity != models.SeverityError {
			t.Errorf("error severity = %q, want %q", got[0].Severity, models.SeverityError)
		}
		if got[1].Severity != models.SeverityWarning {
			t.Errorf("warning severity = %q, want %q", got[1].Severity, models.SeverityWarning)
		}
		if got[2].Severity != models.SeverityInfo {
			t.Errorf("info severity = %q, want %q", got[2].Severity, models.SeverityInfo)
		}
	})
}
