package analyzer

import (
	"fmt"
	"testing"

	"pad-core/models"
)

// buildLargeFlow constructs a synthetic flow with `n` blocks of mixed types and
// a small pool of reused variables, so the data-flow rules (uninitialized- and
// unused-variable) exercise their reader/writer indexes the way a real flow
// does. Used by the large-flow benchmark below and handy for ad-hoc profiling
// via RunAnalysis (which fills report.RuleProfiles with per-rule timings).
func buildLargeFlow(n int) *models.FlowDocument {
	types := []struct {
		bt      models.BlockType
		rawType string
		name    string
	}{
		{models.BlockTypeAction, "WebAutomation.Click.Click", "Click button"},
		{models.BlockTypeAction, "Variables.SetVariable", "Set %var%"},
		{models.BlockTypeAction, "File.ReadFromFile", "Read C:\\data\\in.txt"},
		{models.BlockTypeAction, "HTTP.Request", "GET http://example.com/api"},
		{models.BlockTypeCondition, "Conditionals.If", "If %x% > 0"},
		{models.BlockTypeLoop, "Loop.Loop", "Loop %i% from 1 to 100"},
	}
	blocks := make([]models.Block, 0, n)
	for i := range n {
		tp := types[i%len(types)]
		blk := makeBlock(
			fmt.Sprintf("b%d", i),
			fmt.Sprintf("%s %d", tp.name, i),
			tp.bt,
			tp.rawType,
			0,
		)
		blk.SubflowID = "sf1"
		blk.LineNumber = i + 1
		// A small variable pool means each variable is read by many blocks —
		// the shape that made the uninitialized-variable rule O(readers²).
		blk.Variables = []string{fmt.Sprintf("var%d", i%50)}
		blocks = append(blocks, *blk)
	}
	return &models.FlowDocument{
		ID:       "perf",
		Name:     "Perf",
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: blocks}},
	}
}

// BenchmarkRunAnalysisLarge measures end-to-end analysis on a large flow via the
// production core path (profiling disabled), as a guard against re-introducing a
// super-linear rule. Run: go test ./core/analyzer/ -bench RunAnalysisLarge -run x
func BenchmarkRunAnalysisLarge(b *testing.B) {
	flow := buildLargeFlow(8000)
	rules := AllRules()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = runAnalysisCore(flow, rules, nil, nil, false)
	}
}
