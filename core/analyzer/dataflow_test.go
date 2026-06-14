package analyzer

import (
	"fmt"
	"sort"
	"testing"

	"pad-core/models"
)

// sortedEqual reports whether a and b contain the same elements (as multisets).
func sortedEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	ac := append([]string(nil), a...)
	bc := append([]string(nil), b...)
	sort.Strings(ac)
	sort.Strings(bc)
	for i := range ac {
		if ac[i] != bc[i] {
			return false
		}
	}
	return true
}

// TestDataFlowIndexParity verifies the index-backed findReaders/findWriters/
// findReadersAfter produce the same sets as a naive full-scan reference. Guards
// the O(blocks²)→O(blocks) reimplementation against behavioral drift.
func TestDataFlowIndexParity(t *testing.T) {
	mk := func(id, raw string, line int, out string, vars ...string) models.Block {
		b := makeBlock(id, id, models.BlockTypeAction, raw, 0)
		b.SubflowID = "sf1"
		b.LineNumber = line
		if out != "" {
			b.Properties = map[string]string{"_output": out}
		}
		b.Variables = vars
		return *b
	}
	blocks := []models.Block{
		mk("w1", "Variables.SetVariable", 1, "X"),
		mk("r1", "Display.ShowMessage", 2, "", "X"),
		mk("w2", "Variables.SetVariable", 3, "Y", "X"), // reads X, writes Y
		mk("self", "Variables.SetVariable", 4, "Z", "Z"), // writes and reads Z (self-assign)
		mk("r2", "Display.ShowMessage", 5, "", "Y", "Z"),
	}
	flow := &models.FlowDocument{ID: "t", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: blocks}}}
	ctx := buildContext(flow, nil)

	naiveReaders := func(v string) []string {
		var out []string
		for _, b := range ctx.AllBlocks {
			if b.Properties != nil && (b.Properties["_output"] == v || b.Properties["_var"] == v) {
				continue
			}
			for _, vv := range b.Variables {
				if vv == v {
					out = append(out, b.ID)
					break
				}
			}
		}
		return out
	}
	naiveWriters := func(v, exclude string) []string {
		var out []string
		for id, b := range ctx.AllBlocks {
			if id == exclude || b.Properties == nil {
				continue
			}
			if b.Properties["_output"] == v {
				out = append(out, id)
			}
			if b.Properties["_var"] == v {
				out = append(out, id)
			}
		}
		return out
	}
	naiveReadersAfter := func(v, writerID string) []string {
		wb := ctx.AllBlocks[writerID]
		var out []string
		for id, b := range ctx.AllBlocks {
			if id == writerID || b.LineNumber <= wb.LineNumber {
				continue
			}
			for _, vv := range b.Variables {
				if vv == v {
					out = append(out, id)
					break
				}
			}
		}
		return out
	}

	for _, v := range []string{"X", "Y", "Z", "W"} {
		if got, want := findReaders(v, ctx), naiveReaders(v); !sortedEqual(got, want) {
			t.Errorf("findReaders(%q) = %v, naive = %v", v, got, want)
		}
		if got, want := findWriters(v, ctx, ""), naiveWriters(v, ""); !sortedEqual(got, want) {
			t.Errorf("findWriters(%q) = %v, naive = %v", v, got, want)
		}
		if got, want := findWriters(v, ctx, "w2"), naiveWriters(v, "w2"); !sortedEqual(got, want) {
			t.Errorf("findWriters(%q, exclude w2) = %v, naive = %v", v, got, want)
		}
	}
	if got, want := findReadersAfter("X", ctx, "w1"), naiveReadersAfter("X", "w1"); !sortedEqual(got, want) {
		t.Errorf("findReadersAfter(X, w1) = %v, naive = %v", got, want)
	}
}

// BenchmarkAnalyzeDataFlow exercises the data-flow analysis on a sizable
// synthetic flow. Before the index rewrite, findReaders/findWriters scanned all
// blocks per variable, making this O(blocks²·vars).
func BenchmarkAnalyzeDataFlow(b *testing.B) {
	const n = 2000
	blocks := make([]models.Block, 0, n)
	for i := 0; i < n; i++ {
		blk := makeBlock(fmt.Sprintf("b%d", i), fmt.Sprintf("Set v%d", i), models.BlockTypeVariable, "Variables.SetVariable", 0)
		blk.SubflowID = "sf1"
		blk.LineNumber = i
		blk.Properties = map[string]string{"_output": fmt.Sprintf("v%d", i)}
		// Each block reads the previous block's variable, creating a chain.
		if i > 0 {
			blk.Variables = []string{fmt.Sprintf("v%d", i-1)}
		}
		blocks = append(blocks, *blk)
	}
	flow := &models.FlowDocument{ID: "bench", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: blocks}}}

	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = AnalyzeDataFlow(flow)
	}
}

func TestSubflowMismatchRule(t *testing.T) {
	rule := &SubflowMismatchRule{}

	t.Run("call without output capture emits finding when subflow has outputs", func(t *testing.T) {
		callBlock := makeBlock("b1", "Call Helper", models.BlockTypeSubflow, "CALL", 0)
		callBlock.SubflowID = "sf1"
		callBlock.Properties = map[string]string{}
		target := models.Subflow{
			ID:   "sf2",
			Name: "Helper",
			Variables: []models.VariableDecl{
				{Name: "Output_Result", Type: "string", Scope: "output"},
			},
		}
		flow := &models.FlowDocument{
			ID:       "test",
			Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*callBlock}}, target},
		}
		ctx := buildContext(flow, nil)
		got := rule.Check(callBlock, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
		if got[0].RuleID != "subflow-mismatch" {
			t.Errorf("ruleID = %q", got[0].RuleID)
		}
	})

	t.Run("call with output capture emits no finding", func(t *testing.T) {
		callBlock := makeBlock("b1", "Call Helper", models.BlockTypeSubflow, "CALL", 0)
		callBlock.SubflowID = "sf1"
		callBlock.Properties = map[string]string{"_output": "result"}
		target := models.Subflow{
			ID:   "sf2",
			Name: "Helper",
			Variables: []models.VariableDecl{
				{Name: "Output_Result", Type: "string", Scope: "output"},
			},
		}
		flow := &models.FlowDocument{
			ID:       "test",
			Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*callBlock}}, target},
		}
		ctx := buildContext(flow, nil)
		got := rule.Check(callBlock, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("non-subflow block emits nothing", func(t *testing.T) {
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

func TestDeadDataRule(t *testing.T) {
	rule := &DeadDataRule{}

	t.Run("variable with all readers dead emits finding", func(t *testing.T) {
		setVar := makeBlock("b1", "Set MyVar", models.BlockTypeVariable, "Variables.SetVariable", 0)
		setVar.SubflowID = "sf1"
		setVar.Properties = map[string]string{"_output": "MyVar", "_value": "hello"}
		exitBlock := makeBlock("b2", "End flow", models.BlockTypeAction, "EndFlow", 0)
		exitBlock.SubflowID = "sf1"
		reader := makeBlock("b3", "Read", models.BlockTypeAction, "Display.ShowMessage", 0)
		reader.SubflowID = "sf1"
		reader.Variables = []string{"MyVar"}
		cond := makeBlock("cond1", "If true", models.BlockTypeCondition, "IF", 0)
		cond.SubflowID = "sf1"
		cond.Children = []models.Block{*setVar, *exitBlock, *reader}
		flow := &models.FlowDocument{
			ID:       "test",
			Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*cond}}},
		}
		ctx := buildContext(flow, nil)
		got := rule.Check(setVar, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
		if got[0].RuleID != "dead-data" {
			t.Errorf("ruleID = %q", got[0].RuleID)
		}
	})

	t.Run("variable with live reader emits no finding", func(t *testing.T) {
		setVar := makeBlock("b1", "Set MyVar", models.BlockTypeVariable, "Variables.SetVariable", 0)
		setVar.SubflowID = "sf1"
		setVar.Properties = map[string]string{"_output": "MyVar", "_value": "hello"}
		reader := makeBlock("b3", "Read", models.BlockTypeAction, "Display.ShowMessage", 0)
		reader.SubflowID = "sf1"
		reader.Variables = []string{"MyVar"}
		flow := &models.FlowDocument{
			ID:       "test",
			Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*setVar, *reader}}},
		}
		ctx := buildContext(flow, nil)
		got := rule.Check(setVar, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("non-variable block emits nothing", func(t *testing.T) {
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

func TestAnalyzeDataFlow(t *testing.T) {
	t.Run("empty flow returns empty analysis", func(t *testing.T) {
		doc := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main"}}}
		df := AnalyzeDataFlow(doc)
		if df == nil {
			t.Fatal("expected non-nil result")
		}
		if len(df.Blocks) != 0 {
			t.Errorf("expected 0 blocks, got %d", len(df.Blocks))
		}
	})

	t.Run("block writes are tracked", func(t *testing.T) {
		b := makeBlock("b1", "Set X", models.BlockTypeVariable, "Variables.SetVariable", 0)
		b.SubflowID = "sf1"
		b.Properties = map[string]string{"_output": "X", "_value": "42"}
		doc := &models.FlowDocument{
			ID:       "test",
			Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}},
		}
		df := AnalyzeDataFlow(doc)
		bdf := df.Blocks["b1"]
		if bdf == nil {
			t.Fatal("expected block data flow")
		}
		if len(bdf.Writes) != 1 || bdf.Writes[0] != "X" {
			t.Errorf("writes = %v, want [X]", bdf.Writes)
		}
	})
}
