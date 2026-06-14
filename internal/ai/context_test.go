package ai

import (
	"context"
	"strings"
	"testing"

	"pad-core/models"
)

// stubProvider satisfies the Provider interface with just EstimateTokens.
type stubProvider struct{}

func (stubProvider) SupportsTools() bool             { return false }
func (stubProvider) ID() string                      { return "stub" }
func (stubProvider) Name() string                    { return "Stub" }
func (stubProvider) Models(_ context.Context) ([]ModelInfo, error) { return nil, nil }
func (stubProvider) DefaultModel() string            { return "" }
func (stubProvider) FreeModel() string               { return "" }
func (stubProvider) ContextLimit() int               { return 100_000 }
func (stubProvider) PricePerMillionTokens() Pricing  { return Pricing{} }
func (stubProvider) Embed(_ context.Context, _ []string) ([][]float32, error) { return nil, nil }
func (stubProvider) EstimateTokens(text string) int  { return EstimateTokens(text) }
func (stubProvider) Chat(_ context.Context, _ Request) (*Response, error) {
	return nil, nil
}
func (stubProvider) Stream(_ context.Context, _ Request, _ func(Chunk)) error {
	return nil
}

// makeMinimalDoc returns the smallest valid FlowDocument.
func makeMinimalDoc(name string) *models.FlowDocument {
	return &models.FlowDocument{
		ID:   "doc-1",
		Name: name,
		Metadata: models.FlowMetadata{
			BlockCount:   0,
			SubflowCount: 0,
		},
		BlocksByID:   make(map[string]*models.Block),
		SubflowsByID: make(map[string]*models.Subflow),
	}
}

func TestBuildContext_SystemPromptNeverEmpty(t *testing.T) {
	req := ContextRequest{
		Flow:        makeMinimalDoc("MyFlow"),
		TokenBudget: 8000,
		Provider:    stubProvider{},
	}
	sysPrompt, ctx := BuildContext(req)

	if sysPrompt == "" {
		t.Error("system prompt must not be empty")
	}
	if !strings.Contains(sysPrompt, "Power Automate") {
		t.Errorf("system prompt should reference Power Automate, got: %q", sysPrompt)
	}
	if !strings.Contains(ctx, "MyFlow") {
		t.Errorf("context message should contain flow name, got: %q", ctx)
	}
}

func TestBuildContext_SelectedBlock_AppearsInContext(t *testing.T) {
	block := &models.Block{
		ID:         "b1",
		Name:       "Click Submit Button",
		Type:       models.BlockTypeAction,
		RawType:    "MouseAndKeyboard.Click",
		LineNumber: 10,
		Properties: map[string]string{"Control": "MyControl"},
		Variables:  []string{"Result"},
		SubflowID:  "sf1",
	}
	doc := makeMinimalDoc("ClickFlow")
	doc.BlocksByID["b1"] = block

	req := ContextRequest{
		Flow:          doc,
		SelectedBlock: block,
		TokenBudget:   8000,
		Provider:      stubProvider{},
	}
	_, ctx := BuildContext(req)

	if !strings.Contains(ctx, "Click Submit Button") {
		t.Errorf("context should contain selected block name, got:\n%s", ctx)
	}
	if !strings.Contains(ctx, "MouseAndKeyboard.Click") {
		t.Errorf("context should contain raw block type, got:\n%s", ctx)
	}
}

func TestBuildContext_SelectedBlock_FindingsIncluded(t *testing.T) {
	block := &models.Block{ID: "b1", Name: "Loop", Type: models.BlockTypeLoop, SubflowID: "sf1"}
	doc := makeMinimalDoc("LoopFlow")
	doc.BlocksByID["b1"] = block

	findings := []models.Finding{
		{BlockID: "b1", Severity: models.SeverityError, Title: "Infinite loop risk", Description: "No exit condition."},
		{BlockID: "b2", Severity: models.SeverityWarning, Title: "Unrelated", Description: "Different block."},
	}

	req := ContextRequest{
		Flow:          doc,
		SelectedBlock: block,
		Findings:      findings,
		TokenBudget:   8000,
		Provider:      stubProvider{},
	}
	_, ctx := BuildContext(req)

	// The block-scoped "Known Issues" section must include the block's finding.
	if !strings.Contains(ctx, "Infinite loop risk") {
		t.Errorf("context should include finding for the selected block, got:\n%s", ctx)
	}
	// The block-scoped section must NOT list findings for other blocks.
	knownIssuesIdx := strings.Index(ctx, "Known Issues with This Block")
	summaryIdx := strings.Index(ctx, "Analysis Summary")
	if knownIssuesIdx != -1 && summaryIdx > knownIssuesIdx {
		blockSection := ctx[knownIssuesIdx:summaryIdx]
		if strings.Contains(blockSection, "Unrelated") {
			t.Errorf("block findings section should not include findings from other blocks:\n%s", blockSection)
		}
	}
}

func TestBuildContext_SourceFiles_IncludedInContext(t *testing.T) {
	doc := makeMinimalDoc("SrcFlow")
	req := ContextRequest{
		Flow:           doc,
		RawSourceFiles: map[string]string{"main.txt": "LAUNCH Browser\nCLICK btn"},
		TokenBudget:    8000,
		Provider:       stubProvider{},
	}
	_, ctx := BuildContext(req)

	if !strings.Contains(ctx, "main.txt") {
		t.Errorf("context should include source file name, got:\n%s", ctx)
	}
	if !strings.Contains(ctx, "LAUNCH Browser") {
		t.Errorf("context should include source file content, got:\n%s", ctx)
	}
}

func TestBuildContext_TokenBudget_Respected(t *testing.T) {
	// A very tight budget forces truncation.
	doc := makeMinimalDoc("BigFlow")
	req := ContextRequest{
		Flow:        doc,
		TokenBudget: 5, // extremely small
		Provider:    stubProvider{},
	}
	_, ctx := BuildContext(req)

	// Context must be within roughly the char limit (budget * 3.5).
	budget := 5
	maxChars := int(float64(budget) * 3.5)
	if len(ctx) > maxChars+5 { // +5 for "..." suffix and rounding
		t.Errorf("context too long: %d chars, want <= %d (budget=5 tokens)", len(ctx), maxChars)
	}
}

func TestFilterFindingsForBlock(t *testing.T) {
	findings := []models.Finding{
		{BlockID: "a", Title: "issue-a"},
		{BlockID: "b", Title: "issue-b"},
		{BlockID: "a", Title: "issue-a2"},
	}

	got := filterFindingsForBlock(findings, "a")
	if len(got) != 2 {
		t.Fatalf("expected 2 findings for block a, got %d", len(got))
	}
	for _, f := range got {
		if f.BlockID != "a" {
			t.Errorf("unexpected blockID %q in filtered results", f.BlockID)
		}
	}

	none := filterFindingsForBlock(findings, "z")
	if len(none) != 0 {
		t.Errorf("expected 0 findings for unknown block, got %d", len(none))
	}
}

func TestFindParentChain_NilSafe(t *testing.T) {
	// None of these should panic.
	chain := findParentChain(nil, nil)
	if len(chain) != 0 {
		t.Errorf("nil doc: expected empty chain, got %d", len(chain))
	}

	doc := makeMinimalDoc("X")
	chain = findParentChain(doc, nil)
	if len(chain) != 0 {
		t.Errorf("nil block: expected empty chain, got %d", len(chain))
	}
}

func TestFindParentChain_SingleParent(t *testing.T) {
	parent := &models.Block{ID: "p1", Name: "Loop", Type: models.BlockTypeLoop}
	child := &models.Block{ID: "c1", Name: "Action", Type: models.BlockTypeAction, ParentID: "p1"}

	doc := makeMinimalDoc("ChainFlow")
	doc.BlocksByID["p1"] = parent
	doc.BlocksByID["c1"] = child

	chain := findParentChain(doc, child)
	if len(chain) != 1 {
		t.Fatalf("expected 1 parent, got %d", len(chain))
	}
	if chain[0].ID != "p1" {
		t.Errorf("parent ID = %q, want %q", chain[0].ID, "p1")
	}
}

func TestFindParentChain_DeepChain(t *testing.T) {
	grandparent := &models.Block{ID: "g", Name: "GP", ParentID: ""}
	parent := &models.Block{ID: "p", Name: "P", ParentID: "g"}
	child := &models.Block{ID: "c", Name: "C", ParentID: "p"}

	doc := makeMinimalDoc("DeepFlow")
	doc.BlocksByID["g"] = grandparent
	doc.BlocksByID["p"] = parent
	doc.BlocksByID["c"] = child

	chain := findParentChain(doc, child)
	if len(chain) != 2 {
		t.Fatalf("expected 2 ancestors, got %d", len(chain))
	}
	// Chain should be top-down: grandparent first, then parent.
	if chain[0].ID != "g" || chain[1].ID != "p" {
		t.Errorf("chain order wrong: got [%s, %s], want [g, p]", chain[0].ID, chain[1].ID)
	}
}

func TestBuildContext_SelectedSubflow_AppearsInContext(t *testing.T) {
	sf := &models.Subflow{
		ID:   "sf1",
		Name: "MySubflow",
		Blocks: []models.Block{
			{ID: "b1", Name: "Click Button", Type: models.BlockTypeAction},
			{ID: "b2", Name: "Set Var", Type: models.BlockTypeVariable},
		},
	}
	doc := makeMinimalDoc("SubflowFlow")
	doc.SubflowsByID["sf1"] = sf

	req := ContextRequest{
		Flow:            doc,
		SelectedSubflow: sf,
		TokenBudget:     8000,
		Provider:        stubProvider{},
	}
	_, ctx := BuildContext(req)

	if !strings.Contains(ctx, "MySubflow") {
		t.Errorf("context should contain subflow name, got:\n%s", ctx)
	}
}

func TestFindSiblings_NilSafe(t *testing.T) {
	// Neither nil doc nor nil block should panic.
	if got := findSiblings(nil, nil); got != nil {
		t.Errorf("nil doc: expected nil, got %v", got)
	}
	doc := makeMinimalDoc("X")
	if got := findSiblings(doc, nil); got != nil {
		t.Errorf("nil block: expected nil, got %v", got)
	}
}

func TestFindSiblings_TopLevel(t *testing.T) {
	sf := &models.Subflow{
		ID:   "sf1",
		Name: "Main",
		Blocks: []models.Block{
			{ID: "a", SubflowID: "sf1"},
			{ID: "b", SubflowID: "sf1"},
			{ID: "c", SubflowID: "sf1"},
		},
	}
	doc := makeMinimalDoc("SiblingFlow")
	doc.SubflowsByID["sf1"] = sf

	target := &sf.Blocks[1] // block "b"
	siblings := findSiblings(doc, target)

	if len(siblings) != 2 {
		t.Fatalf("expected 2 siblings for block b, got %d", len(siblings))
	}
	for _, s := range siblings {
		if s.ID == target.ID {
			t.Error("target block should not appear in its own sibling list")
		}
	}
}

func TestFindSiblings_NestedBlock(t *testing.T) {
	parent := &models.Block{
		ID: "p",
		Children: []models.Block{
			{ID: "c1"},
			{ID: "c2"},
			{ID: "c3"},
		},
		SubflowID: "sf1",
	}
	doc := makeMinimalDoc("NestedFlow")
	doc.BlocksByID["p"] = parent

	target := &parent.Children[0] // c1
	target.ParentID = "p"
	siblings := findSiblings(doc, target)

	if len(siblings) != 2 {
		t.Fatalf("expected 2 siblings for c1, got %d", len(siblings))
	}
	for _, s := range siblings {
		if s.ID == "c1" {
			t.Error("c1 should not appear in its own sibling list")
		}
	}
}
