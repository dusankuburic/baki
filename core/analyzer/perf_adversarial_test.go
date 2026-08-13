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
