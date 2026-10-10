package analyzer

import (
	"context"
	"testing"
)

// TestBuildContext_StructuralIndexesEagerFactsLazy pins the split between the
// structural indexes buildContext builds (what the walk and AnalyzeDataFlowCtx
// read) and the rule-only facts, which stay unbuilt until an accessor asks.
func TestBuildContext_StructuralIndexesEagerFactsLazy(t *testing.T) {
	flow := buildDeadDataFanoutFlow(3, 3)
	ctx := buildContext(flow, nil)

	if len(ctx.AllBlocks) != 7 {
		t.Fatalf("AllBlocks: %d, want 7", len(ctx.AllBlocks))
	}
	if ctx.TerminatorIndex["sf1"] != 3 {
		t.Errorf("TerminatorIndex[sf1] = %d, want 3", ctx.TerminatorIndex["sf1"])
	}
	if len(ctx.WritersByVar["X"]) != 3 || len(ctx.ReadersByVar["X"]) != 3 {
		t.Errorf("variable indexes: writers %d, readers %d", len(ctx.WritersByVar["X"]), len(ctx.ReadersByVar["X"]))
	}
	if ctx.FirstReaderByVar["X"] != "r0" {
		t.Errorf("FirstReaderByVar[X] = %q, want r0", ctx.FirstReaderByVar["X"])
	}
	if ctx.SubflowByID["sf1"] == nil || ctx.SubflowByName["Main"] == nil {
		t.Errorf("subflow indexes missing: byID %v byName %v", ctx.SubflowByID, ctx.SubflowByName)
	}
	if built := builtFacts(ctx); len(built) != 0 {
		t.Fatalf("buildContext built rule-only facts %v", built)
	}

	// Each accessor builds its own group and nothing else.
	if len(ctx.HasErrorHandler()) != 7 {
		t.Errorf("HasErrorHandler has %d entries, want 7", len(ctx.HasErrorHandler()))
	}
	if got := builtFacts(ctx); len(got) != 1 || got[0] != "hasErrorHandler" {
		t.Errorf("after HasErrorHandler(): built %v", got)
	}
	if !ctx.AllReadersDead()["X"] {
		t.Errorf("AllReadersDead[X] = false, want true")
	}
	if ctx.blockSig(ctx.AllBlocks["w0"]) == "" || !ctx.signatures.built || len(ctx.signatures.v) != 7 {
		t.Errorf("signatures: built %v, %d entries", ctx.signatures.built, len(ctx.signatures.v))
	}
	forceAllFacts(ctx)
	if got := builtFacts(ctx); len(got) != len(factGroups) {
		t.Errorf("after forcing every accessor: built %v, want all %d groups", got, len(factGroups))
	}
}

// TestAnalyzeDataFlow_UsesIndexContext pins that data flow builds only the
// structural context and the one fact it reads (UsedVariables). The
// observable is allocation count: the whole of AnalyzeDataFlowCtx must
// allocate less than a context with every fact forced (one content signature
// per block, the ancestor flags, the taint fixed point, the call graph); a
// data-flow pass that forced the rule facts would allocate more.
func TestAnalyzeDataFlow_UsesIndexContext(t *testing.T) {
	flow := buildNestedLoopFlow(250, 8)

	full := testing.AllocsPerRun(5, func() { forceAllFacts(buildContext(flow, nil)) })
	dataFlow := testing.AllocsPerRun(5, func() {
		if _, err := AnalyzeDataFlowCtx(context.Background(), flow, DefaultDataFlowLimits); err != nil {
			t.Fatal(err)
		}
	})
	t.Logf("allocs: context with every fact %.0f, AnalyzeDataFlowCtx %.0f (%.2fx)", full, dataFlow, dataFlow/full)
	if dataFlow >= full {
		t.Fatalf("AnalyzeDataFlowCtx allocated %.0f times, a context with every fact %.0f — "+
			"data flow is forcing rule-only facts", dataFlow, full)
	}

	res, err := AnalyzeDataFlowCtx(context.Background(), flow, DefaultDataFlowLimits)
	if err != nil {
		t.Fatal(err)
	}
	if want := len(buildContext(flow, nil).AllBlocks); len(res.Blocks) != want {
		t.Errorf("data flow covers %d blocks, context has %d", len(res.Blocks), want)
	}
}
