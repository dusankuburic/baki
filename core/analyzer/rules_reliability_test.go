package analyzer

import (
	"fmt"
	"testing"

	"pad-core/models"
)

func TestMissingTimeoutRule(t *testing.T) {
	rule := &MissingTimeoutRule{}

	t.Run("HTTP action without timeout emits finding", func(t *testing.T) {
		b := makeBlock("b1", "HTTP Request", models.BlockTypeAction, "Http.Invoke", 0)
		b.SubflowID = "sf1"
		b.Properties = map[string]string{"url": "https://example.com"}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
		if got[0].RuleID != "missing-timeout" {
			t.Errorf("ruleID = %q", got[0].RuleID)
		}
	})

	t.Run("HTTP action with timeout emits no finding", func(t *testing.T) {
		b := makeBlock("b1", "HTTP Request", models.BlockTypeAction, "Http.Invoke", 0)
		b.SubflowID = "sf1"
		b.Properties = map[string]string{"url": "https://example.com", "timeoutInSeconds": "30"}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("non-network action emits nothing", func(t *testing.T) {
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

func TestSensitiveDataExposureRule(t *testing.T) {
	rule := &SensitiveDataExposureRule{}

	t.Run("password var in file write emits finding", func(t *testing.T) {
		b := makeBlock("b1", "Write to file", models.BlockTypeAction, "File.Write", 0)
		b.SubflowID = "sf1"
		b.Variables = []string{"api_key", "outputPath"}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
		if got[0].RuleID != "sensitive-exposure" {
			t.Errorf("ruleID = %q", got[0].RuleID)
		}
		if got[0].Severity != models.SeverityError {
			t.Errorf("severity = %q, want error", got[0].Severity)
		}
	})

	t.Run("normal var in file write emits no finding", func(t *testing.T) {
		b := makeBlock("b1", "Write to file", models.BlockTypeAction, "File.Write", 0)
		b.SubflowID = "sf1"
		b.Variables = []string{"outputData", "filePath"}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("non-sink action emits nothing", func(t *testing.T) {
		b := makeBlock("b1", "Set variable", models.BlockTypeAction, "SetVariable.Set", 0)
		b.SubflowID = "sf1"
		b.Variables = []string{"api_key"}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})
}

func TestErrorSwallowRule(t *testing.T) {
	rule := &ErrorSwallowRule{}

	t.Run("error handler with only comments emits finding", func(t *testing.T) {
		comment := makeBlock("c1", "TODO: handle", models.BlockTypeComment, "Comment", 4)
		comment.SubflowID = "sf1"
		handler := makeBlock("h1", "OnError", models.BlockTypeErrorHandler, "OnError.Handler", 0)
		handler.SubflowID = "sf1"
		handler.Children = []models.Block{*comment}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*handler}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(handler, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
		if got[0].RuleID != "error-swallow" {
			t.Errorf("ruleID = %q", got[0].RuleID)
		}
	})

	t.Run("error handler with action inside emits no finding", func(t *testing.T) {
		action := makeBlock("a1", "Log error", models.BlockTypeAction, "Logger.Log", 4)
		action.SubflowID = "sf1"
		handler := makeBlock("h1", "OnError", models.BlockTypeErrorHandler, "OnError.Handler", 0)
		handler.SubflowID = "sf1"
		handler.Children = []models.Block{*action}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*handler}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(handler, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("empty error handler emits no finding (handled by empty-handler rule)", func(t *testing.T) {
		handler := makeBlock("h1", "OnError", models.BlockTypeErrorHandler, "OnError.Handler", 0)
		handler.SubflowID = "sf1"
		handler.Children = []models.Block{}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*handler}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(handler, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})
}

func TestMissingRetryRule(t *testing.T) {
	rule := &MissingRetryRule{}

	t.Run("HTTP action without handler emits finding", func(t *testing.T) {
		b := makeBlock("b1", "HTTP Request", models.BlockTypeAction, "Http.Invoke", 0)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
		if got[0].RuleID != "missing-retry" {
			t.Errorf("ruleID = %q", got[0].RuleID)
		}
	})

	t.Run("HTTP action with error handler emits no finding", func(t *testing.T) {
		action := makeBlock("b2", "HTTP Request", models.BlockTypeAction, "Http.Invoke", 4)
		action.SubflowID = "sf1"
		handler := makeBlock("h1", "OnError", models.BlockTypeErrorHandler, "OnError.Handler", 0)
		handler.SubflowID = "sf1"
		handler.Children = []models.Block{*action}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*handler}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(action, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("non-network action emits nothing", func(t *testing.T) {
		b := makeBlock("b1", "Set variable", models.BlockTypeAction, "SetVariable.Set", 0)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	// Regression: an action already wrapped in a retry loop must NOT be flagged.
	// Before the fix, isInsideRetryLoop computed hasRetry then discarded it
	// (`_ = hasRetry; return false`), so this case produced a false positive.
	t.Run("HTTP action inside retry loop emits no finding", func(t *testing.T) {
		action := makeBlock("b1", "HTTP Request", models.BlockTypeAction, "Http.Invoke", 4)
		action.SubflowID = "sf1"
		loop := makeBlock("loop1", "Retry loop", models.BlockTypeLoop, "Loop.Loop", 0)
		loop.SubflowID = "sf1"
		loop.Children = []models.Block{*action}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*loop}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(action, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings for action inside retry loop, got %d", len(got))
		}
	})

	// A loop whose name carries no retry/attempt hint should NOT suppress the
	// finding — confirms the fix walks up rather than blanket-suppressing on any loop.
	t.Run("HTTP action inside plain loop still emits finding", func(t *testing.T) {
		action := makeBlock("b1", "HTTP Request", models.BlockTypeAction, "Http.Invoke", 4)
		action.SubflowID = "sf1"
		loop := makeBlock("loop1", "For each item", models.BlockTypeLoop, "Loop.ForEach", 0)
		loop.SubflowID = "sf1"
		loop.Children = []models.Block{*action}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*loop}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(action, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding for action inside non-retry loop, got %d", len(got))
		}
	})
}

func TestWideLoopRule(t *testing.T) {
	rule := &WideLoopRule{}

	t.Run("small loop emits no finding", func(t *testing.T) {
		children := make([]models.Block, 10)
		for i := range children {
			children[i] = *makeBlock(fmt.Sprintf("c%d", i), "Action", models.BlockTypeAction, "SetVariable.Set", 4)
			children[i].SubflowID = "sf1"
		}
		loop := makeBlock("loop1", "Loop", models.BlockTypeLoop, "Loop.ForEach", 0)
		loop.SubflowID = "sf1"
		loop.Children = children
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*loop}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(loop, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("large loop emits finding", func(t *testing.T) {
		children := make([]models.Block, 25)
		for i := range children {
			children[i] = *makeBlock(fmt.Sprintf("c%d", i), "Action", models.BlockTypeAction, "SetVariable.Set", 4)
			children[i].SubflowID = "sf1"
		}
		loop := makeBlock("loop1", "Loop", models.BlockTypeLoop, "Loop.ForEach", 0)
		loop.SubflowID = "sf1"
		loop.Children = children
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*loop}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(loop, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
		if got[0].RuleID != "wide-loop" {
			t.Errorf("ruleID = %q", got[0].RuleID)
		}
	})

	t.Run("non-loop block emits nothing", func(t *testing.T) {
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
