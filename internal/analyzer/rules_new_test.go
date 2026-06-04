package analyzer

import (
	"testing"

	"pad-analyzer/internal/models"
)

func TestGotoAntipatternRule(t *testing.T) {
	rule := &GotoAntipatternRule{}

	t.Run("GOTO that jumps across loop boundary emits finding", func(t *testing.T) {
		label := makeBlock("lbl1", "MyLabel", models.BlockTypeAction, "LABEL", 8)
		label.SubflowID = "sf1"
		gotoBlock := makeBlock("g1", "Goto MyLabel", models.BlockTypeAction, "GOTO", 4)
		gotoBlock.Properties = map[string]string{"_target": "MyLabel"}
		gotoBlock.SubflowID = "sf1"
		loopBody := makeBlock("b2", "Do work", models.BlockTypeAction, "SetVariable.Set", 8)
		loopBody.SubflowID = "sf1"
		loop := makeBlock("loop1", "Loop ForEach", models.BlockTypeLoop, "Loop.ForEach", 0)
		loop.SubflowID = "sf1"
		loop.Children = []models.Block{*gotoBlock, *loopBody}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*label, *loop}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(gotoBlock, ctx)
		if len(got) == 0 {
			t.Fatal("expected finding for cross-scope GOTO")
		}
		if got[0].RuleID != "goto-antipattern" {
			t.Errorf("ruleID = %q", got[0].RuleID)
		}
	})

	t.Run("non-GOTO block emits nothing", func(t *testing.T) {
		b := makeBlock("b1", "Set variable", models.BlockTypeAction, "SetVariable.Set", 0)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})
}

func TestEmptyBranchRule(t *testing.T) {
	rule := &EmptyBranchRule{}

	t.Run("empty else branch emits finding", func(t *testing.T) {
		elseBranch := makeBlock("else1", "Else", models.BlockTypeElse, "ELSE", 4)
		elseBranch.SubflowID = "sf1"
		cond := makeBlock("cond1", "If condition", models.BlockTypeCondition, "IF", 0)
		cond.SubflowID = "sf1"
		cond.Children = []models.Block{*elseBranch}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*cond}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(cond, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
		if got[0].RuleID != "empty-branch" {
			t.Errorf("ruleID = %q", got[0].RuleID)
		}
	})

	t.Run("else branch with actions emits no finding", func(t *testing.T) {
		action := makeBlock("b1", "Do something", models.BlockTypeAction, "SetVariable.Set", 8)
		action.SubflowID = "sf1"
		elseBranch := makeBlock("else1", "Else", models.BlockTypeElse, "ELSE", 4)
		elseBranch.SubflowID = "sf1"
		elseBranch.Children = []models.Block{*action}
		cond := makeBlock("cond1", "If condition", models.BlockTypeCondition, "IF", 0)
		cond.SubflowID = "sf1"
		cond.Children = []models.Block{*elseBranch}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*cond}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(cond, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("non-condition block emits nothing", func(t *testing.T) {
		b := makeBlock("b1", "Do work", models.BlockTypeAction, "SetVariable.Set", 0)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})
}

func TestRedundantActionRule(t *testing.T) {
	rule := &RedundantActionRule{}

	t.Run("variable set to itself emits finding", func(t *testing.T) {
		b := makeBlock("b1", "Set MyVar", models.BlockTypeVariable, "Variables.SetVariable", 0)
		b.SubflowID = "sf1"
		b.Properties = map[string]string{
			"_output": "MyVar",
			"_value":  "%MyVar%",
		}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) == 0 {
			t.Fatal("expected finding for self-assignment")
		}
		if got[0].RuleID != "redundant-action" {
			t.Errorf("ruleID = %q", got[0].RuleID)
		}
	})

	t.Run("variable set to different value emits no finding", func(t *testing.T) {
		b := makeBlock("b1", "Set MyVar", models.BlockTypeVariable, "Variables.SetVariable", 0)
		b.SubflowID = "sf1"
		b.Properties = map[string]string{
			"_output": "MyVar",
			"_value":  "%OtherVar%",
		}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("non-variable block emits nothing for self-assignment", func(t *testing.T) {
		b := makeBlock("b1", "Do work", models.BlockTypeAction, "SetVariable.Set", 0)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})
}

func TestFileOpNoErrorHandlerRule(t *testing.T) {
	rule := &FileOpNoErrorHandlerRule{}

	t.Run("file read without handler emits finding", func(t *testing.T) {
		b := makeBlock("b1", "Read from file", models.BlockTypeAction, "File.Read", 0)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
		if got[0].RuleID != "file-op-no-error-handler" {
			t.Errorf("ruleID = %q", got[0].RuleID)
		}
	})

	t.Run("file write with error handler emits no finding", func(t *testing.T) {
		action := makeBlock("b2", "Write to file", models.BlockTypeAction, "File.Write", 4)
		action.SubflowID = "sf1"
		handler := makeBlock("b1", "OnError", models.BlockTypeErrorHandler, "OnError.Handler", 0)
		handler.SubflowID = "sf1"
		handler.Children = []models.Block{*action}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*handler}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(action, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("non-file action emits nothing", func(t *testing.T) {
		b := makeBlock("b1", "Set variable", models.BlockTypeAction, "SetVariable.Set", 0)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("folder create without handler emits finding", func(t *testing.T) {
		b := makeBlock("b1", "Create folder", models.BlockTypeAction, "Folder.Create", 0)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding for folder operation, got %d", len(got))
		}
	})
}

func TestComputeFlowMetrics(t *testing.T) {
	t.Run("empty flow returns zero metrics", func(t *testing.T) {
		doc := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main"}}}
		m := ComputeFlowMetrics(doc, nil)
		if m.TotalBlocks != 0 {
			t.Errorf("TotalBlocks = %d, want 0", m.TotalBlocks)
		}
		if m.HealthScore != 100 {
			t.Errorf("HealthScore = %d, want 100", m.HealthScore)
		}
	})

	t.Run("flow with findings reduces health score", func(t *testing.T) {
		b := makeBlock("b1", "Click", models.BlockTypeAction, "WebAutomation.Click", 0)
		b.SubflowID = "sf1"
		doc := &models.FlowDocument{
			ID:       "test",
			Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}},
		}
		report := &models.AnalysisReport{
			Findings: []models.Finding{
				{Severity: models.SeverityError},
				{Severity: models.SeverityWarning},
			},
		}
		m := ComputeFlowMetrics(doc, report)
		if m.HealthScore >= 100 {
			t.Errorf("HealthScore = %d, expected < 100", m.HealthScore)
		}
	})

	t.Run("cyclomatic complexity counts decisions", func(t *testing.T) {
		elseBr := makeBlock("else1", "Else", models.BlockTypeElse, "ELSE", 4)
		elseBr.SubflowID = "sf1"
		cond := makeBlock("cond1", "If x", models.BlockTypeCondition, "IF", 0)
		cond.SubflowID = "sf1"
		cond.Children = []models.Block{*elseBr}
		loop := makeBlock("loop1", "Loop", models.BlockTypeLoop, "Loop.ForEach", 0)
		loop.SubflowID = "sf1"
		doc := &models.FlowDocument{
			ID:       "test",
			Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*cond, *loop}}},
		}
		m := ComputeFlowMetrics(doc, nil)
		if len(m.Subflows) != 1 {
			t.Fatalf("expected 1 subflow metric, got %d", len(m.Subflows))
		}
		sf := m.Subflows[0]
		if sf.CyclomaticComplexity < 4 {
			t.Errorf("CyclomaticComplexity = %d, want >= 4 (1 base + if + else + loop)", sf.CyclomaticComplexity)
		}
	})
}
