package analyzer

import (
	"testing"

	"pad-analyzer/internal/models"
)

func TestUnhandledErrorRule(t *testing.T) {
	rule := &UnhandledErrorRule{}

	t.Run("fallible action without handler emits finding", func(t *testing.T) {
		b := makeBlock("b1", "Click button", models.BlockTypeAction, "WebAutomation.Click", 0)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
		if got[0].RuleID != "unhandled-error" {
			t.Errorf("ruleID = %q", got[0].RuleID)
		}
	})

	t.Run("non-fallible action emits no finding", func(t *testing.T) {
		b := makeBlock("b1", "Set variable", models.BlockTypeAction, "SetVariable.Set", 0)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("fallible action inside error handler emits no finding", func(t *testing.T) {
		action := makeBlock("b2", "Click", models.BlockTypeAction, "WebAutomation.Click", 4)
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

	t.Run("Http action is fallible", func(t *testing.T) {
		b := makeBlock("b1", "HTTP Request", models.BlockTypeAction, "Http.Invoke", 0)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
	})

	t.Run("non-action block type emits nothing", func(t *testing.T) {
		b := makeBlock("b1", "Loop", models.BlockTypeLoop, "Loop.ForEach", 0)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})
}

func TestInfiniteLoopRiskRule(t *testing.T) {
	rule := &InfiniteLoopRiskRule{}

	t.Run("loop without exit emits finding", func(t *testing.T) {
		child := makeBlock("b2", "Do something", models.BlockTypeAction, "SetVariable.Set", 4)
		loop := makeBlock("b1", "Loop ForEach", models.BlockTypeLoop, "Loop.ForEach", 0)
		loop.SubflowID = "sf1"
		loop.Children = []models.Block{*child}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*loop}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(loop, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
	})

	t.Run("loop with Exit loop emits no finding", func(t *testing.T) {
		exit := makeBlock("b2", "Exit loop", models.BlockTypeAction, "ExitLoop.Exit", 4)
		loop := makeBlock("b1", "Loop", models.BlockTypeLoop, "Loop.ForEach", 0)
		loop.SubflowID = "sf1"
		loop.Children = []models.Block{*exit}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*loop}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(loop, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("non-loop block emits nothing", func(t *testing.T) {
		b := makeBlock("b1", "Action", models.BlockTypeAction, "SetVariable.Set", 0)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("loop with Return emits no finding", func(t *testing.T) {
		ret := makeBlock("b2", "Return value", models.BlockTypeAction, "Return.Value", 4)
		loop := makeBlock("b1", "Loop", models.BlockTypeLoop, "Loop.ForEach", 0)
		loop.SubflowID = "sf1"
		loop.Children = []models.Block{*ret}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*loop}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(loop, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})
}

func TestDeepNestingRule(t *testing.T) {
	rule := &DeepNestingRule{}

	// wrap creates a condition container that holds child as its only child.
	wrap := func(id string, child models.Block) *models.Block {
		p := makeBlock(id, "C", models.BlockTypeCondition, "IF", 0)
		p.SubflowID = "sf1"
		p.Children = []models.Block{child}
		return p
	}

	t.Run("block at tree depth 7 emits finding", func(t *testing.T) {
		leaf := makeBlock("leaf", "Deep", models.BlockTypeAction, "SetVariable.Set", 0)
		leaf.SubflowID = "sf1"
		// leaf is wrapped by 7 containers → tree depth 7 (> default maxDepth 6)
		root := wrap("c0", *wrap("c1", *wrap("c2", *wrap("c3", *wrap("c4", *wrap("c5", *wrap("c6", *leaf)))))))
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*root}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(ctx.AllBlocks["leaf"], ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
	})

	t.Run("block at tree depth 6 emits no finding", func(t *testing.T) {
		leaf := makeBlock("leaf", "OK", models.BlockTypeAction, "SetVariable.Set", 0)
		leaf.SubflowID = "sf1"
		// leaf is wrapped by 6 containers → tree depth 6 (= default maxDepth 6, not exceeded)
		root := wrap("c0", *wrap("c1", *wrap("c2", *wrap("c3", *wrap("c4", *wrap("c5", *leaf))))))
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*root}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(ctx.AllBlocks["leaf"], ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("custom maxDepth from settings", func(t *testing.T) {
		leaf := makeBlock("leaf", "Deep", models.BlockTypeAction, "SetVariable.Set", 0)
		leaf.SubflowID = "sf1"
		settings := models.DefaultSettings()
		settings.Analysis.Rules["deep-nesting"] = models.RuleConfig{
			Enabled:  true,
			Severity: "info",
			Options:  map[string]interface{}{"maxDepth": float64(3)},
		}
		// leaf wrapped by 4 containers → tree depth 4 (> maxDepth 3)
		root := wrap("c0", *wrap("c1", *wrap("c2", *wrap("c3", *leaf))))
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*root}}}}
		ctx := buildContext(flow, settings)
		got := rule.Check(ctx.AllBlocks["leaf"], ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding with maxDepth=3, got %d", len(got))
		}
	})
}

func TestHardcodedCredentialRule(t *testing.T) {
	rule := &HardcodedCredentialRule{}

	t.Run("password pattern emits finding", func(t *testing.T) {
		b := makeBlock("b1", "Set variable", models.BlockTypeAction, "SetVariable.Set", 0)
		b.Properties = map[string]string{"Value": "password='mysecret123'"}
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) < 1 {
			t.Fatalf("expected >= 1 finding, got %d", len(got))
		}
	})

	t.Run("AWS key pattern emits finding", func(t *testing.T) {
		b := makeBlock("b1", "Set variable", models.BlockTypeAction, "SetVariable.Set", 0)
		b.Properties = map[string]string{"Key": "AKIAIOSFODNN7EXAMPLE"}
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) < 1 {
			t.Fatalf("expected >= 1 finding, got %d", len(got))
		}
	})

	t.Run("clean value emits no finding", func(t *testing.T) {
		b := makeBlock("b1", "Set variable", models.BlockTypeAction, "SetVariable.Set", 0)
		b.Properties = map[string]string{"Value": "hello world"}
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("high entropy string emits finding", func(t *testing.T) {
		b := makeBlock("b1", "Set var", models.BlockTypeAction, "SetVariable.Set", 0)
		b.Properties = map[string]string{"Value": "aB3dE7fG9hJ2kL5mN8pQrS4tUvW6xYz0AbCdEfGh"}
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) < 1 {
			t.Fatalf("expected >= 1 finding for high entropy, got %d", len(got))
		}
	})
}

func TestDeadCodeRule(t *testing.T) {
	rule := &DeadCodeRule{}

	t.Run("block after Exit subflow emits finding", func(t *testing.T) {
		b1 := makeBlock("b1", "Exit subflow", models.BlockTypeAction, "ExitSubflow.Exit", 0)
		b1.SubflowID = "sf1"
		b2 := makeBlock("b2", "Unreachable", models.BlockTypeAction, "SetVariable.Set", 0)
		b2.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b1, *b2}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b2, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
	})

	t.Run("block before exit emits no finding", func(t *testing.T) {
		b1 := makeBlock("b1", "Do work", models.BlockTypeAction, "SetVariable.Set", 0)
		b1.SubflowID = "sf1"
		b2 := makeBlock("b2", "Exit subflow", models.BlockTypeAction, "ExitSubflow.Exit", 0)
		b2.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b1, *b2}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b1, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("single block emits no finding", func(t *testing.T) {
		b := makeBlock("b1", "Solo", models.BlockTypeAction, "SetVariable.Set", 0)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})
}

func TestMissingDelayRule(t *testing.T) {
	rule := &MissingDelayRule{}

	t.Run("consecutive web actions emit finding", func(t *testing.T) {
		b1 := makeBlock("b1", "Click link", models.BlockTypeAction, "WebAutomation.Click", 0)
		b1.SubflowID = "sf1"
		b2 := makeBlock("b2", "Get text", models.BlockTypeAction, "WebAutomation.GetText", 0)
		b2.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b1, *b2}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b2, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
	})

	t.Run("web action with wait before emits no finding", func(t *testing.T) {
		b1 := makeBlock("b1", "Click link", models.BlockTypeAction, "WebAutomation.Click", 0)
		b1.SubflowID = "sf1"
		b2 := makeBlock("b2", "Wait", models.BlockTypeAction, "WebAutomation.WaitForElement", 0)
		b2.SubflowID = "sf1"
		b3 := makeBlock("b3", "Get text", models.BlockTypeAction, "WebAutomation.GetText", 0)
		b3.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b1, *b2, *b3}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b3, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("non-web action emits no finding", func(t *testing.T) {
		b1 := makeBlock("b1", "Set var", models.BlockTypeAction, "SetVariable.Set", 0)
		b1.SubflowID = "sf1"
		b2 := makeBlock("b2", "Set var 2", models.BlockTypeAction, "SetVariable.Set", 0)
		b2.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b1, *b2}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b2, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})
}

func TestDuplicateActionRule(t *testing.T) {
	rule := &DuplicateActionRule{}

	t.Run("three identical actions emit finding", func(t *testing.T) {
		b1 := makeBlock("b1", "Click", models.BlockTypeAction, "WebAutomation.Click", 0)
		b1.Properties = map[string]string{"Target": "btn1"}
		b1.SubflowID = "sf1"
		b2 := makeBlock("b2", "Click", models.BlockTypeAction, "WebAutomation.Click", 0)
		b2.Properties = map[string]string{"Target": "btn1"}
		b2.SubflowID = "sf1"
		b3 := makeBlock("b3", "Click", models.BlockTypeAction, "WebAutomation.Click", 0)
		b3.Properties = map[string]string{"Target": "btn1"}
		b3.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b1, *b2, *b3}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b1, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
	})

	t.Run("two identical actions emit no finding", func(t *testing.T) {
		b1 := makeBlock("b1", "Click", models.BlockTypeAction, "WebAutomation.Click", 0)
		b1.Properties = map[string]string{"Target": "btn1"}
		b1.SubflowID = "sf1"
		b2 := makeBlock("b2", "Click", models.BlockTypeAction, "WebAutomation.Click", 0)
		b2.Properties = map[string]string{"Target": "btn1"}
		b2.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b1, *b2}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b1, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("different actions emit no finding", func(t *testing.T) {
		b1 := makeBlock("b1", "Click", models.BlockTypeAction, "WebAutomation.Click", 0)
		b1.Properties = map[string]string{"Target": "btn1"}
		b1.SubflowID = "sf1"
		b2 := makeBlock("b2", "Click", models.BlockTypeAction, "WebAutomation.Click", 0)
		b2.Properties = map[string]string{"Target": "btn2"}
		b2.SubflowID = "sf1"
		b3 := makeBlock("b3", "Click", models.BlockTypeAction, "WebAutomation.Click", 0)
		b3.Properties = map[string]string{"Target": "btn1"}
		b3.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b1, *b2, *b3}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b1, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})
}

func TestUnusedVariableRule(t *testing.T) {
	rule := &UnusedVariableRule{}

	t.Run("unused variable emits finding", func(t *testing.T) {
		b := makeBlock("b1", "Set MyVar", models.BlockTypeVariable, "SetVariable.Set", 0)
		b.Properties = map[string]string{"_output": "MyVar"}
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
	})

	t.Run("used variable emits no finding", func(t *testing.T) {
		b1 := makeBlock("b1", "Set MyVar", models.BlockTypeVariable, "SetVariable.Set", 0)
		b1.Properties = map[string]string{"_output": "MyVar"}
		b1.SubflowID = "sf1"
		b2 := makeBlock("b2", "Use var", models.BlockTypeAction, "SetVariable.Set", 0)
		b2.Properties = map[string]string{"Value": "%MyVar%"}
		b2.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b1, *b2}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b1, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("non-variable block emits nothing", func(t *testing.T) {
		b := makeBlock("b1", "Action", models.BlockTypeAction, "SetVariable.Set", 0)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})
}

func TestSlowPatternRule(t *testing.T) {
	rule := &SlowPatternRule{}

	t.Run("UI automation in loop without delay emits finding", func(t *testing.T) {
		child := makeBlock("b2", "Click", models.BlockTypeAction, "WebAutomation.Click", 4)
		loop := makeBlock("b1", "Loop", models.BlockTypeLoop, "Loop.ForEach", 0)
		loop.SubflowID = "sf1"
		loop.Children = []models.Block{*child}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*loop}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(loop, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
	})

	t.Run("UI automation in loop with delay emits no finding", func(t *testing.T) {
		click := makeBlock("b2", "Click", models.BlockTypeAction, "WebAutomation.Click", 4)
		wait := makeBlock("b3", "Wait", models.BlockTypeAction, "Wait.Delay", 4)
		loop := makeBlock("b1", "Loop", models.BlockTypeLoop, "Loop.ForEach", 0)
		loop.SubflowID = "sf1"
		loop.Children = []models.Block{*click, *wait}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*loop}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(loop, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("non-UI loop emits no finding", func(t *testing.T) {
		child := makeBlock("b2", "Set var", models.BlockTypeAction, "SetVariable.Set", 4)
		loop := makeBlock("b1", "Loop", models.BlockTypeLoop, "Loop.ForEach", 0)
		loop.SubflowID = "sf1"
		loop.Children = []models.Block{*child}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*loop}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(loop, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})
}

func TestEmptyHandlerRule(t *testing.T) {
	rule := &EmptyHandlerRule{}

	t.Run("empty handler emits finding", func(t *testing.T) {
		b := makeBlock("b1", "OnError", models.BlockTypeErrorHandler, "OnError.Handler", 0)
		b.SubflowID = "sf1"
		b.Children = []models.Block{}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
	})

	t.Run("handler with children emits no finding", func(t *testing.T) {
		child := makeBlock("b2", "Log error", models.BlockTypeAction, "SetVariable.Set", 4)
		b := makeBlock("b1", "OnError", models.BlockTypeErrorHandler, "OnError.Handler", 0)
		b.SubflowID = "sf1"
		b.Children = []models.Block{*child}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("non-error-handler block emits nothing", func(t *testing.T) {
		b := makeBlock("b1", "Action", models.BlockTypeAction, "SetVariable.Set", 0)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})
}

func TestResourceLeakRule(t *testing.T) {
	rule := &ResourceLeakRule{}

	t.Run("open file without close emits finding", func(t *testing.T) {
		opener := makeBlock("b1", "Open file", models.BlockTypeAction, "File.OpenTextFile", 0)
		opener.Properties = map[string]string{"_output": "FileHandle"}
		opener.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*opener}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(opener, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
		if got[0].RuleID != "resource-leak" {
			t.Errorf("ruleID = %q", got[0].RuleID)
		}
	})

	t.Run("open file with matching close emits no finding", func(t *testing.T) {
		opener := makeBlock("b1", "Open file", models.BlockTypeAction, "File.OpenTextFile", 0)
		opener.Properties = map[string]string{"_output": "FileHandle"}
		opener.SubflowID = "sf1"

		closer := makeBlock("b2", "Close file", models.BlockTypeAction, "File.CloseTextFile", 0)
		closer.Variables = []string{"FileHandle"}
		closer.SubflowID = "sf1"

		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*opener, *closer}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(opener, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("non-open action emits nothing", func(t *testing.T) {
		b := makeBlock("b1", "Set var", models.BlockTypeAction, "SetVariable.Set", 0)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("open without _output emits nothing", func(t *testing.T) {
		opener := makeBlock("b1", "Open file", models.BlockTypeAction, "File.OpenTextFile", 0)
		opener.Properties = map[string]string{} // no _output
		opener.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*opener}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(opener, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings (no output var), got %d", len(got))
		}
	})
}

func TestHardcodedCredentialRule_VariableName(t *testing.T) {
	rule := &HardcodedCredentialRule{}

	t.Run("variable named Password with literal value emits finding", func(t *testing.T) {
		b := makeBlock("b1", "Set Password", models.BlockTypeVariable, "SET", 0)
		b.Properties = map[string]string{
			"_var":    "Password",
			"_output": "Password",
			"_value":  "mysecret123",
		}
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) == 0 {
			t.Fatal("expected at least 1 finding for credential-named variable with literal value")
		}
	})

	t.Run("variable named Password with variable ref emits no finding", func(t *testing.T) {
		b := makeBlock("b1", "Set Password", models.BlockTypeVariable, "SET", 0)
		b.Properties = map[string]string{
			"_var":    "Password",
			"_output": "Password",
			"_value":  "%SecureInput%",
		}
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		// %SecureInput% contains %, so it should NOT be flagged
		for _, f := range got {
			if f.Title == "Credential variable set to literal value" {
				t.Fatal("should not flag variable references as hardcoded credentials")
			}
		}
	})
}

func TestUninitializedVariableRule(t *testing.T) {
	rule := &UninitializedVariableRule{}

	t.Run("variable never assigned emits finding on first use", func(t *testing.T) {
		b := makeBlock("b1", "Use ghost", models.BlockTypeAction, "SetVariable.Set", 0)
		b.Variables = []string{"GhostVar"}
		b.SubflowID = "sf1"
		b.LineNumber = 1
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
		if got[0].RuleID != "uninitialized-variable" {
			t.Errorf("ruleID = %q", got[0].RuleID)
		}
	})

	t.Run("variable assigned before use emits no finding", func(t *testing.T) {
		setter := makeBlock("b1", "Set MyVar", models.BlockTypeVariable, "SET", 0)
		setter.Properties = map[string]string{"_output": "MyVar"}
		setter.SubflowID = "sf1"
		setter.LineNumber = 1

		user := makeBlock("b2", "Use MyVar", models.BlockTypeAction, "SetVariable.Set", 0)
		user.Variables = []string{"MyVar"}
		user.SubflowID = "sf1"
		user.LineNumber = 2

		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*setter, *user}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(user, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("system variable emits no finding", func(t *testing.T) {
		b := makeBlock("b1", "Use sys", models.BlockTypeAction, "SetVariable.Set", 0)
		b.Variables = []string{"CurrentDateTime"}
		b.SubflowID = "sf1"
		b.LineNumber = 1
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings for system var, got %d", len(got))
		}
	})

	t.Run("Input_ prefixed variable emits no finding", func(t *testing.T) {
		b := makeBlock("b1", "Use input", models.BlockTypeAction, "SetVariable.Set", 0)
		b.Variables = []string{"Input_UserName"}
		b.SubflowID = "sf1"
		b.LineNumber = 1
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings for Input_ var, got %d", len(got))
		}
	})

	t.Run("only first usage block emits finding, second does not", func(t *testing.T) {
		b1 := makeBlock("b1", "First use", models.BlockTypeAction, "SetVariable.Set", 0)
		b1.Variables = []string{"Orphan"}
		b1.SubflowID = "sf1"
		b1.LineNumber = 1

		b2 := makeBlock("b2", "Second use", models.BlockTypeAction, "SetVariable.Set", 0)
		b2.Variables = []string{"Orphan"}
		b2.SubflowID = "sf1"
		b2.LineNumber = 2

		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b1, *b2}}}}
		ctx := buildContext(flow, nil)

		got1 := rule.Check(b1, ctx)
		if len(got1) != 1 {
			t.Fatalf("expected 1 finding on first use, got %d", len(got1))
		}
		got2 := rule.Check(b2, ctx)
		if len(got2) != 0 {
			t.Fatalf("expected 0 findings on second use (deduplicated), got %d", len(got2))
		}
	})
}

func TestAllRulesIntegration(t *testing.T) {
	exit := makeBlock("b1", "Exit subflow", models.BlockTypeAction, "ExitSubflow.Exit", 0)
	exit.SubflowID = "sf1"
	dead := makeBlock("b2", "Dead code", models.BlockTypeAction, "SetVariable.Set", 0)
	dead.SubflowID = "sf1"

	flow := &models.FlowDocument{
		ID:       "test",
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*exit, *dead}}},
	}

	report := RunAnalysis(flow, AllRules(), nil, nil)

	ruleCounts := make(map[string]int)
	for _, f := range report.Findings {
		ruleCounts[f.RuleID]++
	}

	if ruleCounts["dead-code"] < 1 {
		t.Errorf("expected dead-code finding, got counts: %+v", ruleCounts)
	}
}
