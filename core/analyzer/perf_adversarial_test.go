package analyzer

import (
	"context"
	"fmt"
	"testing"

	"pad-core/models"
)

// buildResourceOpenFlow builds a flow of N resource-open actions with no close,
// the shape that made the resource-leak rule O(opens × blocks).
func buildResourceOpenFlow(n int) *models.FlowDocument {
	blocks := make([]models.Block, 0, n)
	for i := range n {
		b := makeBlock(fmt.Sprintf("b%d", i), fmt.Sprintf("Open file %d", i), models.BlockTypeAction, "File.OpenTextFile", 0)
		b.SubflowID = "sf1"
		b.LineNumber = i + 1
		b.Properties = map[string]string{"_output": fmt.Sprintf("handle%d", i)}
		blocks = append(blocks, *b)
	}
	return &models.FlowDocument{ID: "res", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: blocks}}}
}

// buildGotoFlow builds a flow of N GOTO actions, the shape that made the
// goto-antipattern rule O(gotos × blocks).
func buildGotoFlow(n int) *models.FlowDocument {
	blocks := make([]models.Block, 0, n)
	for i := range n {
		b := makeBlock(fmt.Sprintf("b%d", i), fmt.Sprintf("Goto L%d", i), models.BlockTypeAction, "GOTO", 0)
		b.SubflowID = "sf1"
		b.LineNumber = i + 1
		b.Properties = map[string]string{"_target": fmt.Sprintf("Label%d", i)}
		blocks = append(blocks, *b)
	}
	return &models.FlowDocument{ID: "goto", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: blocks}}}
}

// These two guard the resource-leak and goto-antipattern rules against
// re-introducing their per-block full-flow scans (O(matches × blocks)).
func BenchmarkResourceLeakLarge(b *testing.B) {
	flow := buildResourceOpenFlow(8000)
	rules := AllRules()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = runAnalysisCore(context.Background(), flow, rules, nil, nil, false)
	}
}

func BenchmarkGotoLarge(b *testing.B) {
	flow := buildGotoFlow(8000)
	rules := AllRules()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = runAnalysisCore(context.Background(), flow, rules, nil, nil, false)
	}
}

// buildManySubflowsFlow builds a flow of n subflows (each a few action blocks),
// with n CALL blocks in the first subflow targeting the others by name. This is
// the shape that made the subflow rules O(subflows²) (large-subflow,
// subflow-no-error-handler resolve their subflow by ID) and O(calls × subflows)
// (subflow-mismatch resolves the CALL target by name) before SubflowByID /
// SubflowByName were precomputed once in buildContext.
func buildManySubflowsFlow(n int) *models.FlowDocument {
	line := 0
	subflows := make([]models.Subflow, 0, n)
	for i := range n {
		sfID := fmt.Sprintf("sf%d", i)
		blocks := make([]models.Block, 0, 5)
		// 4 plain action blocks so the subflow is "non-trivial" (> 3 blocks),
		// tripping subflow-no-error-handler and feeding large-subflow's count.
		for k := range 4 {
			line++
			b := makeBlock(fmt.Sprintf("b%d_%d", i, k), fmt.Sprintf("Click %d", k), models.BlockTypeAction, "WebAutomation.Click.Click", 0)
			b.SubflowID = sfID
			b.LineNumber = line
			blocks = append(blocks, *b)
		}
		// The first subflow also issues a CALL to every subflow by name, so
		// subflow-mismatch resolves n targets against the n-subflow index.
		if i == 0 {
			for j := range n {
				line++
				c := makeBlock(fmt.Sprintf("call%d", j), fmt.Sprintf("Call Sub%d", j), models.BlockTypeAction, "CALL", 0)
				c.SubflowID = sfID
				c.LineNumber = line
				blocks = append(blocks, *c)
			}
		}
		subflows = append(subflows, models.Subflow{ID: sfID, Name: fmt.Sprintf("Sub%d", i), Blocks: blocks})
	}
	return &models.FlowDocument{ID: "manysub", Subflows: subflows}
}

// BenchmarkSubflowRulesLarge guards the subflow rules against re-introducing
// their per-invocation full-subflow scans (O(subflows²) / O(calls × subflows)).
func BenchmarkSubflowRulesLarge(b *testing.B) {
	flow := buildManySubflowsFlow(2000)
	rules := AllRules()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = runAnalysisCore(context.Background(), flow, rules, nil, nil, false)
	}
}

// buildLabelHeavyFlow builds a flow of n LABEL blocks (names drawn from a
// small pool so most are duplicated) plus filler actions — the shape that made
// duplicate-label O(labels × blocks): every LABEL Check scanned AllBlocks and
// ToLower'd each scanned block's name.
func buildLabelHeavyFlow(labels, filler int) *models.FlowDocument {
	total := labels + filler
	blocks := make([]models.Block, 0, total)
	line := 0
	for i := range labels {
		line++
		b := makeBlock(fmt.Sprintf("lbl%d", i), fmt.Sprintf("Label%d", i%100), models.BlockTypeAction, "LABEL", 0)
		b.SubflowID = "sf1"
		b.LineNumber = line
		b.Properties = map[string]string{"_target": fmt.Sprintf("Label%d", i%100)}
		blocks = append(blocks, *b)
	}
	for i := range filler {
		line++
		b := makeBlock(fmt.Sprintf("act%d", i), fmt.Sprintf("Click %d", i), models.BlockTypeAction, "WebAutomation.Click.Click", 0)
		b.SubflowID = "sf1"
		b.LineNumber = line
		blocks = append(blocks, *b)
	}
	return &models.FlowDocument{ID: "labels", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: blocks}}}
}

// BenchmarkDuplicateLabelLarge guards duplicate-label against re-introducing
// its per-LABEL full-flow scan (O(labels × blocks) + an allocation per scanned
// block via strings.ToLower).
func BenchmarkDuplicateLabelLarge(b *testing.B) {
	flow := buildLabelHeavyFlow(1000, 7000)
	rules := AllRules()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = runAnalysisCore(context.Background(), flow, rules, nil, nil, false)
	}
}

// buildNestedLoopFlow builds depth-deep nested LOOPs whose bodies hold UI
// actions and (only in the innermost loop) a WAIT, with `perLevel` actions per
// level — the shape that makes slow-pattern/wide-loop O(n × depth) when each
// ancestor LOOP Check re-walks its entire subtree including nested loops.
func buildNestedLoopFlow(depth, perLevel int) *models.FlowDocument {
	line := 0
	var build func(indent int) []models.Block
	build = func(indent int) []models.Block {
		line++
		loop := makeBlock(fmt.Sprintf("loop-%d", indent), "Loop 1 to 10", models.BlockTypeLoop, "Loop.Loop", indent)
		loop.SubflowID = "sf1"
		loop.LineNumber = line
		kids := make([]models.Block, 0, perLevel+1)
		for i := range perLevel {
			line++
			a := makeBlock(fmt.Sprintf("act-%d-%d", indent, i), fmt.Sprintf("Click %d", i), models.BlockTypeAction, "WebAutomation.Click.Click", indent+1)
			a.SubflowID = "sf1"
			a.LineNumber = line
			kids = append(kids, *a)
		}
		if indent+1 < depth {
			// Recurse one level deeper; the nested loop's blocks become this
			// loop's subtree too — exactly the double-walk shape.
			kids = append(kids, build(indent+1)...)
		} else {
			line++
			w := makeBlock("wait-inner", "Wait 1", models.BlockTypeAction, "WAIT", indent+1)
			w.SubflowID = "sf1"
			w.LineNumber = line
			kids = append(kids, *w)
		}
		loop.Children = kids
		return []models.Block{*loop}
	}
	return &models.FlowDocument{ID: "nested", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: build(0)}}}
}

// BenchmarkNestedLoops guards slow-pattern / wide-loop / deep-nesting against
// re-introducing per-ancestor subtree re-walks (O(n × depth)).
func BenchmarkNestedLoops(b *testing.B) {
	flow := buildNestedLoopFlow(50, 8)
	rules := AllRules()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = runAnalysisCore(context.Background(), flow, rules, nil, nil, false)
	}
}
