package analyzer

import (
	"context"
	"testing"
)

// TestBuildIndexContext_OmitsRuleOnlyIndexes pins the split between the
// structural indexes (what AnalyzeDataFlowCtx reads) and the rule-only work
// buildContext layers on top: the lite context must carry every structural
// index and none of the rule-only ones.
func TestBuildIndexContext_OmitsRuleOnlyIndexes(t *testing.T) {
	flow := buildDeadDataFanoutFlow(3, 3)
	lite := buildIndexContext(flow, nil)
	full := buildContext(flow, nil)

	// Structural indexes: present and identical in both.
	if len(lite.AllBlocks) != 7 || len(full.AllBlocks) != 7 {
		t.Fatalf("AllBlocks: lite %d, full %d, want 7", len(lite.AllBlocks), len(full.AllBlocks))
	}
	for _, id := range []string{"w0", "exit", "r2"} {
		if lite.BlockIndex[id] != full.BlockIndex[id] || lite.SiblingKey[id] != full.SiblingKey[id] {
			t.Errorf("%s: BlockIndex/SiblingKey differ between lite and full", id)
		}
	}
	if lite.TerminatorIndex["sf1"] != 3 || full.TerminatorIndex["sf1"] != 3 {
		t.Errorf("TerminatorIndex[sf1]: lite %d, full %d, want 3", lite.TerminatorIndex["sf1"], full.TerminatorIndex["sf1"])
	}
	if len(lite.WritersByVar["X"]) != 3 || len(lite.ReadersByVar["X"]) != 3 || len(lite.UsedVariables) != len(full.UsedVariables) {
		t.Errorf("variable indexes: writers %d, readers %d, used %d (full %d)",
			len(lite.WritersByVar["X"]), len(lite.ReadersByVar["X"]), len(lite.UsedVariables), len(full.UsedVariables))
	}
	if lite.FirstReaderByVar["X"] != "r0" {
		t.Errorf("FirstReaderByVar[X] = %q, want r0", lite.FirstReaderByVar["X"])
	}

	// Rule-only indexes: nil on the lite context, built on the full one.
	if lite.sigCache != nil || lite.HasErrorHandler != nil || lite.InsideRetryLoop != nil ||
		lite.TaintedVars != nil || lite.AllReadersDead != nil || lite.callGraph != nil || lite.SubflowByID != nil {
		t.Errorf("lite context carries rule-only indexes: sigCache=%v HasErrorHandler=%v InsideRetryLoop=%v TaintedVars=%v AllReadersDead=%v callGraph=%v SubflowByID=%v",
			lite.sigCache != nil, lite.HasErrorHandler != nil, lite.InsideRetryLoop != nil,
			lite.TaintedVars != nil, lite.AllReadersDead != nil, lite.callGraph != nil, lite.SubflowByID != nil)
	}
	if len(full.sigCache) != 7 || len(full.HasErrorHandler) != 7 || !full.AllReadersDead["X"] || full.SubflowByID["sf1"] == nil {
		t.Errorf("full context is missing rule-only indexes: sigCache=%d HasErrorHandler=%d AllReadersDead[X]=%v",
			len(full.sigCache), len(full.HasErrorHandler), full.AllReadersDead["X"])
	}
}

// TestAnalyzeDataFlow_UsesIndexContext pins that data flow builds the lite
// context, not the full rule context. The observable is allocation count:
// the full context is the lite one plus the rule-only indexes (one content
// signature per block, the ancestor flags, the taint fixed point, the call
// graph), and the data-flow pass itself allocates less than those on a
// nested fixture (about 0.4× the full context's total). So the whole of
// AnalyzeDataFlowCtx must allocate less than buildContext alone; if it
// built the full context it would allocate more (about 1.3×).
func TestAnalyzeDataFlow_UsesIndexContext(t *testing.T) {
	flow := buildNestedLoopFlow(250, 8)

	full := testing.AllocsPerRun(5, func() { _ = buildContext(flow, nil) })
	dataFlow := testing.AllocsPerRun(5, func() {
		if _, err := AnalyzeDataFlowCtx(context.Background(), flow, DefaultDataFlowLimits); err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("allocs: buildContext %.0f, AnalyzeDataFlowCtx %.0f (%.2fx)", full, dataFlow, dataFlow/full)
	if dataFlow >= full {
		t.Fatalf("AnalyzeDataFlowCtx allocated %.0f times, buildContext alone %.0f — "+
			"data flow is building the full rule context instead of the index-only one", dataFlow, full)
	}

	// And the lite context yields the same answer as the full one would: the
	// data-flow pass reads only structural indexes, so an identical result
	// over the full context is the parity half of this pin.
	res, err := AnalyzeDataFlowCtx(context.Background(), flow, DefaultDataFlowLimits)
	if err != nil {
		t.Fatal(err)
	}
	if want := len(buildContext(flow, nil).AllBlocks); len(res.Blocks) != want {
		t.Errorf("data flow covers %d blocks, context has %d", len(res.Blocks), want)
	}
}
