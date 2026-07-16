package analyzer

import (
	"testing"

	"pad-core/models"
)

// ── High Cyclomatic Complexity ────────────────────────────────────

func TestHighCyclomaticComplexityRule(t *testing.T) {
	rule := &HighCyclomaticComplexityRule{}

	t.Run("high complexity subflow fires", func(t *testing.T) {
		// Build a subflow with > 20 IF blocks → cyclo > 20
		var blocks []models.Block
		for i := 0; i < 25; i++ {
			b := makeBlock("if"+itoaTest(i), "If", models.BlockTypeCondition, "IF", i)
			b.SubflowID = "sf1"
			blocks = append(blocks, *b)
		}
		sf := models.Subflow{ID: "sf1", Name: "Complex", Blocks: blocks}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{sf}}
		ctx := buildContext(flow, nil)
		got := rule.Check(&blocks[0], ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
		if got[0].RuleID != "high-cyclomatic-complexity" {
			t.Errorf("ruleID = %q", got[0].RuleID)
		}
	})

	t.Run("low complexity subflow does not fire", func(t *testing.T) {
		b := makeBlock("b1", "Action", models.BlockTypeAction, "SET", 0)
		b.SubflowID = "sf1"
		sf := models.Subflow{ID: "sf1", Name: "Simple", Blocks: []models.Block{*b}}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{sf}}
		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings for simple subflow, got %d", len(got))
		}
	})

	t.Run("non-first block does not fire", func(t *testing.T) {
		var blocks []models.Block
		for i := 0; i < 25; i++ {
			b := makeBlock("if"+itoaTest(i), "If", models.BlockTypeCondition, "IF", i)
			b.SubflowID = "sf1"
			blocks = append(blocks, *b)
		}
		sf := models.Subflow{ID: "sf1", Name: "Complex", Blocks: blocks}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{sf}}
		ctx := buildContext(flow, nil)
		got := rule.Check(&blocks[1], ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 on non-first block, got %d", len(got))
		}
	})
}

// ── Uncalled Subflow ──────────────────────────────────────────────

func TestUncalledSubflowRule(t *testing.T) {
	rule := &UncalledSubflowRule{}

	t.Run("uncalled non-entry subflow fires", func(t *testing.T) {
		entry := makeBlock("e1", "Entry", models.BlockTypeAction, "SET", 0)
		entry.SubflowID = "sf-main"
		orphan := makeBlock("o1", "Orphan", models.BlockTypeAction, "SET", 0)
		orphan.SubflowID = "sf-orphan"
		sfMain := makeSubflow("sf-main", "Main", entry)
		sfOrphan := makeSubflow("sf-orphan", "Orphan", orphan)
		flow := makeFlowWithSubflows(sfMain, sfOrphan)

		ctx := buildContext(flow, nil)
		got := rule.Check(orphan, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding for uncalled subflow, got %d", len(got))
		}
		if got[0].RuleID != "uncalled-subflow" {
			t.Errorf("ruleID = %q", got[0].RuleID)
		}
	})

	t.Run("entry subflow does not fire", func(t *testing.T) {
		entry := makeBlock("e1", "Entry", models.BlockTypeAction, "SET", 0)
		entry.SubflowID = "sf-main"
		sfMain := makeSubflow("sf-main", "Main", entry)
		flow := makeFlowWithSubflows(sfMain)

		ctx := buildContext(flow, nil)
		got := rule.Check(entry, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 for entry subflow, got %d", len(got))
		}
	})

	t.Run("called subflow does not fire", func(t *testing.T) {
		call := makeBlock("c1", "Call Helper", models.BlockTypeSubflow, "CALL", 0)
		call.SubflowID = "sf-main"
		entry := makeBlock("e1", "Entry", models.BlockTypeAction, "SET", 0)
		entry.SubflowID = "sf-main"
		helper := makeBlock("h1", "Work", models.BlockTypeAction, "SET", 0)
		helper.SubflowID = "sf-helper"
		sfMain := makeSubflow("sf-main", "Main", call, entry)
		sfHelper := makeSubflow("sf-helper", "Helper", helper)
		flow := makeFlowWithSubflows(sfMain, sfHelper)

		ctx := buildContext(flow, nil)
		got := rule.Check(helper, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 for called subflow, got %d", len(got))
		}
	})
}

// ── Duplicate Subflow Name ────────────────────────────────────────

func TestDuplicateSubflowNameRule(t *testing.T) {
	rule := &DuplicateSubflowNameRule{}

	t.Run("duplicate names fire on both", func(t *testing.T) {
		b1 := makeBlock("b1", "Act1", models.BlockTypeAction, "SET", 0)
		b1.SubflowID = "sf-a"
		b2 := makeBlock("b2", "Act2", models.BlockTypeAction, "SET", 0)
		b2.SubflowID = "sf-b"
		sfA := makeSubflow("sf-a", "Duplicate", b1)
		sfB := makeSubflow("sf-b", "Duplicate", b2)
		flow := makeFlowWithSubflows(sfA, sfB)

		ctx := buildContext(flow, nil)
		gotA := rule.Check(b1, ctx)
		if len(gotA) != 1 {
			t.Fatalf("expected 1 finding for sf-a, got %d", len(gotA))
		}
		gotB := rule.Check(b2, ctx)
		if len(gotB) != 1 {
			t.Fatalf("expected 1 finding for sf-b, got %d", len(gotB))
		}
	})

	t.Run("unique names do not fire", func(t *testing.T) {
		b1 := makeBlock("b1", "Act1", models.BlockTypeAction, "SET", 0)
		b1.SubflowID = "sf-a"
		b2 := makeBlock("b2", "Act2", models.BlockTypeAction, "SET", 0)
		b2.SubflowID = "sf-b"
		sfA := makeSubflow("sf-a", "Alpha", b1)
		sfB := makeSubflow("sf-b", "Beta", b2)
		flow := makeFlowWithSubflows(sfA, sfB)

		ctx := buildContext(flow, nil)
		got := rule.Check(b1, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 for unique name, got %d", len(got))
		}
	})
}

// ── Duplicate Label ───────────────────────────────────────────────

func TestDuplicateLabelRule(t *testing.T) {
	rule := &DuplicateLabelRule{}

	t.Run("duplicate labels fire on first", func(t *testing.T) {
		l1 := makeBlock("l1", "Start", models.BlockTypeAction, "LABEL", 1)
		l1.SubflowID = "sf1"
		l2 := makeBlock("l2", "Start", models.BlockTypeAction, "LABEL", 5)
		l2.SubflowID = "sf1"
		sf := makeSubflow("sf1", "Main", l1, l2)
		flow := makeFlowWithSubflows(sf)

		ctx := buildContext(flow, nil)
		got := rule.Check(l1, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding on first label, got %d", len(got))
		}
		if got[0].RuleID != "duplicate-label" {
			t.Errorf("ruleID = %q", got[0].RuleID)
		}
		// Second label should NOT fire (only first emits)
		got2 := rule.Check(l2, ctx)
		if len(got2) != 0 {
			t.Fatalf("expected 0 on second label, got %d", len(got2))
		}
	})

	t.Run("unique labels do not fire", func(t *testing.T) {
		l1 := makeBlock("l1", "Alpha", models.BlockTypeAction, "LABEL", 1)
		l1.SubflowID = "sf1"
		l2 := makeBlock("l2", "Beta", models.BlockTypeAction, "LABEL", 5)
		l2.SubflowID = "sf1"
		sf := makeSubflow("sf1", "Main", l1, l2)
		flow := makeFlowWithSubflows(sf)

		ctx := buildContext(flow, nil)
		got := rule.Check(l1, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 for unique label, got %d", len(got))
		}
	})

	t.Run("non-label block does not fire", func(t *testing.T) {
		b := makeBlock("b1", "Act", models.BlockTypeAction, "SET", 0)
		b.SubflowID = "sf1"
		sf := makeSubflow("sf1", "Main", b)
		flow := makeFlowWithSubflows(sf)

		ctx := buildContext(flow, nil)
		got := rule.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 for non-label, got %d", len(got))
		}
	})
}

// itoaTest is a local test helper to avoid importing strconv in the test file.
func itoaTest(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}
