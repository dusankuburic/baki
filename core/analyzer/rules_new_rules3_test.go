package analyzer

import (
	"testing"

	"pad-core/models"
)

// ── Switch No Default ─────────────────────────────────────────────

func TestSwitchNoDefaultRule(t *testing.T) {
	rule := &SwitchNoDefaultRule{}

	t.Run("switch with no default fires", func(t *testing.T) {
		case1 := makeBlock("c1", "Case 1", models.BlockTypeCase, "CASE", 2)
		case1.SubflowID = "sf1"
		sw := makeBlock("sw1", "Switch", models.BlockTypeSwitch, "SWITCH", 1)
		sw.SubflowID = "sf1"
		sw.Children = []models.Block{*case1}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*sw}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(sw, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
		if got[0].RuleID != "switch-no-default" {
			t.Errorf("ruleID = %q", got[0].RuleID)
		}
	})

	t.Run("switch with default does not fire", func(t *testing.T) {
		case1 := makeBlock("c1", "Case 1", models.BlockTypeCase, "CASE", 2)
		case1.SubflowID = "sf1"
		def := makeBlock("d1", "Default", models.BlockTypeDefault, "DEFAULT", 3)
		def.SubflowID = "sf1"
		sw := makeBlock("sw1", "Switch", models.BlockTypeSwitch, "SWITCH", 1)
		sw.SubflowID = "sf1"
		sw.Children = []models.Block{*case1, *def}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*sw}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(sw, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("non-switch block does not fire", func(t *testing.T) {
		b := makeBlock("b1", "Act", models.BlockTypeAction, "SET", 1)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 for non-switch, got %d", len(got))
		}
	})
}

// ── Empty Subflow ─────────────────────────────────────────────────

func TestEmptySubflowRule(t *testing.T) {
	rule := &EmptySubflowRule{}

	t.Run("empty subflow fires", func(t *testing.T) {
		comment := makeBlock("c1", "just a comment", models.BlockTypeComment, "COMMENT", 1)
		comment.SubflowID = "sf1"
		sf := makeSubflow("sf1", "Empty", comment)
		flow := makeFlowWithSubflows(sf)
		ctx := buildContext(flow, nil)
		got := rule.Check(comment, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
		if got[0].RuleID != "empty-subflow" {
			t.Errorf("ruleID = %q", got[0].RuleID)
		}
	})

	t.Run("non-empty subflow does not fire", func(t *testing.T) {
		action := makeBlock("a1", "DoSomething", models.BlockTypeAction, "SET", 1)
		action.SubflowID = "sf1"
		sf := makeSubflow("sf1", "Active", action)
		flow := makeFlowWithSubflows(sf)
		ctx := buildContext(flow, nil)
		got := rule.Check(action, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 for non-empty subflow, got %d", len(got))
		}
	})
}

// ── TODO In Comment ───────────────────────────────────────────────

func TestTodoInCommentRule(t *testing.T) {
	rule := &TodoInCommentRule{}

	t.Run("comment with TODO fires", func(t *testing.T) {
		b := makeBlock("c1", "TODO: implement this", models.BlockTypeComment, "COMMENT", 1)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
		if got[0].RuleID != "todo-in-comment" {
			t.Errorf("ruleID = %q", got[0].RuleID)
		}
	})

	t.Run("comment with FIXME fires", func(t *testing.T) {
		b := makeBlock("c1", "FIXME: broken", models.BlockTypeComment, "COMMENT", 1)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
	})

	t.Run("comment without marker does not fire", func(t *testing.T) {
		b := makeBlock("c1", "This is a regular comment", models.BlockTypeComment, "COMMENT", 1)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0, got %d", len(got))
		}
	})

	t.Run("non-comment does not fire", func(t *testing.T) {
		b := makeBlock("b1", "TODO something", models.BlockTypeAction, "SET", 1)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 for non-comment, got %d", len(got))
		}
	})
}

// ── Wait Zero ─────────────────────────────────────────────────────

func TestWaitZeroRule(t *testing.T) {
	rule := &WaitZeroRule{}

	t.Run("WAIT 0 fires", func(t *testing.T) {
		b := makeBlock("w1", "Wait 0", models.BlockTypeWait, "WAIT", 1)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
		if got[0].RuleID != "wait-zero" {
			t.Errorf("ruleID = %q", got[0].RuleID)
		}
		// AutoFix is stamped by the engine from ruleAutoFix map, not by Check
		// directly. Verify the catalog reports the fixer.
		if RuleAutoFix("wait-zero") != "remove-block" {
			t.Errorf("RuleAutoFix(wait-zero) = %q, want remove-block", RuleAutoFix("wait-zero"))
		}
	})

	t.Run("WAIT 5 does not fire", func(t *testing.T) {
		b := makeBlock("w1", "Wait 5", models.BlockTypeWait, "WAIT", 1)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 for WAIT 5, got %d", len(got))
		}
	})

	t.Run("non-WAIT does not fire", func(t *testing.T) {
		b := makeBlock("b1", "SET X TO 0", models.BlockTypeVariable, "SET", 1)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 for non-WAIT, got %d", len(got))
		}
	})
}
