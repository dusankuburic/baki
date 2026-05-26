package ai

import (
	"strings"
	"testing"

	"pad-analyzer/internal/models"
)

// ---- BuildContext: nil flow guard ------------------------------------------

func TestBuildContext_NilFlow_ReturnsDefaultPrompt(t *testing.T) {
	req := ContextRequest{
		Flow:        nil,
		TokenBudget: 8000,
		Provider:    stubProvider{},
	}
	sys, ctx := BuildContext(req)
	if sys != SystemPromptDefault {
		t.Errorf("expected default system prompt, got %q", sys)
	}
	if ctx != "" {
		t.Errorf("expected empty context for nil flow, got %q", ctx)
	}
}

// ---- writeFlowOverview: loop body ------------------------------------------

func TestBuildContext_FlowWithSubflows_IncludesSubflowNames(t *testing.T) {
	// writeFlowOverview iterates doc.Subflows; the loop body is only covered when
	// the slice is non-empty.
	sf1 := models.Subflow{ID: "sf1", Name: "Main", Blocks: []models.Block{}}
	sf2 := models.Subflow{ID: "sf2", Name: "Helper", Blocks: []models.Block{}}
	doc := &models.FlowDocument{
		ID:   "doc-1",
		Name: "MyFlow",
		Metadata: models.FlowMetadata{
			BlockCount:   0,
			SubflowCount: 2,
		},
		Subflows:     []models.Subflow{sf1, sf2},
		BlocksByID:   make(map[string]*models.Block),
		SubflowsByID: make(map[string]*models.Subflow),
	}

	req := ContextRequest{
		Flow:        doc,
		TokenBudget: 8000,
		Provider:    stubProvider{},
	}
	_, ctx := BuildContext(req)

	if !strings.Contains(ctx, "Main") {
		t.Errorf("expected 'Main' subflow in context, got:\n%s", ctx)
	}
	if !strings.Contains(ctx, "Helper") {
		t.Errorf("expected 'Helper' subflow in context, got:\n%s", ctx)
	}
}

// ---- writeSubflowOutline: truncation path ----------------------------------

func TestWriteSubflowOutline_Truncates(t *testing.T) {
	// writeSubflowOutline is package-internal; call directly since tests are in package ai.
	blocks := make([]models.Block, 20)
	for i := range blocks {
		blocks[i] = models.Block{ID: "b", Name: "SomeReallyLongBlockName", Type: models.BlockTypeAction}
	}
	sf := &models.Subflow{ID: "sf1", Name: "Main", Blocks: blocks}

	var b strings.Builder
	// Budget of 1 token guarantees truncation after the first block.
	writeSubflowOutline(&b, sf, 1)

	out := b.String()
	if !strings.Contains(out, "truncated") {
		t.Errorf("expected '... (truncated)' in output with budget=1, got:\n%s", out)
	}
}

// ---- writeBlockDetail: children path ---------------------------------------

func TestBuildContext_SelectedBlock_WithChildren_ShowsNestedCount(t *testing.T) {
	child := &models.Block{ID: "c1", Name: "Inner", Type: models.BlockTypeAction}
	block := &models.Block{
		ID:       "b1",
		Name:     "Outer Loop",
		Type:     models.BlockTypeLoop,
		RawType:  "Loop.ForEach",
		SubflowID: "sf1",
		Children: []models.Block{*child},
	}
	doc := makeMinimalDoc("ChildFlow")
	doc.BlocksByID["b1"] = block

	req := ContextRequest{
		Flow:          doc,
		SelectedBlock: block,
		TokenBudget:   8000,
		Provider:      stubProvider{},
	}
	_, ctx := BuildContext(req)

	if !strings.Contains(strings.ToLower(ctx), "nested") {
		t.Errorf("expected 'nested block(s)' mention for block with children, got:\n%s", ctx)
	}
}

// ---- findParentChain: dangling ParentID (parent not in BlocksByID) ---------

func TestFindParentChain_DanglingParentID_Breaks(t *testing.T) {
	// block has a ParentID pointing to a block that doesn't exist in BlocksByID.
	// findParentChain should break out of the loop gracefully.
	block := &models.Block{ID: "c1", Name: "Orphan", ParentID: "nonexistent"}

	doc := makeMinimalDoc("DanglingFlow")
	doc.BlocksByID["c1"] = block
	// "nonexistent" is NOT in BlocksByID → triggers the `!ok` break

	chain := findParentChain(doc, block)
	if len(chain) != 0 {
		t.Errorf("expected empty chain for dangling ParentID, got %d entries", len(chain))
	}
}

// ---- writeFindingsSummary: multiple occurrences of the same title ----------

func TestBuildContext_Findings_DuplicateTitles_ShowsOccurrenceCount(t *testing.T) {
	doc := makeMinimalDoc("DupFlow")
	findings := []models.Finding{
		{BlockID: "b1", Severity: models.SeverityWarning, Title: "Repeated Issue", Description: "first"},
		{BlockID: "b2", Severity: models.SeverityWarning, Title: "Repeated Issue", Description: "second"},
		{BlockID: "b3", Severity: models.SeverityError, Title: "Unique Error", Description: "only one"},
	}

	req := ContextRequest{
		Flow:        doc,
		Findings:    findings,
		TokenBudget: 8000,
		Provider:    stubProvider{},
	}
	_, ctx := BuildContext(req)

	// The "2 occurrences" branch in writeFindingsSummary should fire.
	if !strings.Contains(ctx, "2 occurrences") {
		t.Errorf("expected '2 occurrences' for duplicate finding title, got:\n%s", ctx)
	}
	// The "1 occurrence" branch should also fire.
	if !strings.Contains(ctx, "1 occurrence") {
		t.Errorf("expected '1 occurrence' for unique finding title, got:\n%s", ctx)
	}
}
