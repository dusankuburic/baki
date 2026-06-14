package analyzer

import (
	"fmt"
	"testing"

	"pad-core/models"
)

// makeSubflow builds a Subflow with the given top-level blocks.
func makeSubflow(id, name string, blocks ...*models.Block) models.Subflow {
	bs := make([]models.Block, len(blocks))
	for i, b := range blocks {
		b.SubflowID = id
		bs[i] = *b
	}
	return models.Subflow{ID: id, Name: name, Blocks: bs}
}

// makeFlowWithSubflows builds a FlowDocument from the given subflows.
func makeFlowWithSubflows(sfs ...models.Subflow) *models.FlowDocument {
	return &models.FlowDocument{
		ID:       "flow1",
		Subflows: sfs,
	}
}

// actionBlocks returns n distinct action blocks with IDs starting from the given
// offset so that multiple calls produce globally unique IDs.
func actionBlocks(n int) []*models.Block { return actionBlocksAt(0, n) }

func actionBlocksAt(offset, n int) []*models.Block {
	out := make([]*models.Block, n)
	for i := range out {
		id := fmt.Sprintf("act%d", offset+i)
		out[i] = makeBlock(id, "Action "+id, models.BlockTypeAction, "SetVariable.Set", 0)
	}
	return out
}

func TestSubflowNoErrorHandlerRule_Identity(t *testing.T) {
	r := &SubflowNoErrorHandlerRule{}
	if r.ID() != "subflow-no-error-handler" {
		t.Errorf("ID = %q", r.ID())
	}
	if r.Name() == "" {
		t.Error("Name is empty")
	}
	if r.Description() == "" {
		t.Error("Description is empty")
	}
	if r.DefaultSeverity() != models.SeverityInfo {
		t.Errorf("DefaultSeverity = %q, want info", r.DefaultSeverity())
	}
	if r.Category() != "Reliability" {
		t.Errorf("Category = %q, want Reliability", r.Category())
	}
}

// ---- sfNeedsErrorHandler ---------------------------------------------------

func TestSfNeedsErrorHandler_TrivialSubflow_ReturnsFalse(t *testing.T) {
	blocks := []models.Block{
		*makeBlock("b1", "A1", models.BlockTypeAction, "SetVariable.Set", 0),
		*makeBlock("b2", "A2", models.BlockTypeAction, "SetVariable.Set", 0),
	}
	if sfNeedsErrorHandler(blocks) {
		t.Error("expected false for subflow with only 2 action blocks")
	}
}

func TestSfNeedsErrorHandler_ExactlyThreeActions_ReturnsFalse(t *testing.T) {
	var blocks []models.Block
	for i := 0; i < 3; i++ {
		b := makeBlock(string(rune('a'+i)), "A", models.BlockTypeAction, "SetVariable.Set", 0)
		blocks = append(blocks, *b)
	}
	if sfNeedsErrorHandler(blocks) {
		t.Error("expected false for subflow with exactly 3 action blocks")
	}
}

func TestSfNeedsErrorHandler_FourActions_ReturnsTrue(t *testing.T) {
	var blocks []models.Block
	for i := 0; i < 4; i++ {
		b := makeBlock(string(rune('a'+i)), "A", models.BlockTypeAction, "SetVariable.Set", 0)
		blocks = append(blocks, *b)
	}
	if !sfNeedsErrorHandler(blocks) {
		t.Error("expected true for subflow with 4 action blocks")
	}
}

func TestSfNeedsErrorHandler_CommentsOnly_ReturnsFalse(t *testing.T) {
	var blocks []models.Block
	for i := 0; i < 10; i++ {
		b := makeBlock(string(rune('a'+i)), "Comment", models.BlockTypeComment, "", 0)
		blocks = append(blocks, *b)
	}
	if sfNeedsErrorHandler(blocks) {
		t.Error("expected false for comment-only subflow")
	}
}

func TestSfNeedsErrorHandler_VariablesOnly_ReturnsFalse(t *testing.T) {
	var blocks []models.Block
	for i := 0; i < 5; i++ {
		b := makeBlock(string(rune('a'+i)), "Var", models.BlockTypeVariable, "", 0)
		blocks = append(blocks, *b)
	}
	if sfNeedsErrorHandler(blocks) {
		t.Error("expected false for variable-only subflow")
	}
}

func TestSfNeedsErrorHandler_NestedActions_CountedRecursively(t *testing.T) {
	// Top level has 1 loop, but it contains 4 actions as children → threshold exceeded.
	loop := makeBlock("loop1", "Loop", models.BlockTypeLoop, "Loop.ForEach", 0)
	for i := 0; i < 4; i++ {
		child := *makeBlock(string(rune('a'+i)), "Act", models.BlockTypeAction, "SetVariable.Set", 4)
		loop.Children = append(loop.Children, child)
	}
	if !sfNeedsErrorHandler([]models.Block{*loop}) {
		t.Error("expected true when nested blocks exceed threshold")
	}
}

// ---- SubflowNoErrorHandlerRule.Check ---------------------------------------

func TestSubflowNoErrorHandlerRule_TrivialSubflow_NoFinding(t *testing.T) {
	rule := &SubflowNoErrorHandlerRule{}
	abs := actionBlocks(2)
	sf := makeSubflow("sf1", "Main", abs...)
	flow := makeFlowWithSubflows(sf)
	ctx := buildContext(flow, nil)

	var findings []models.Finding
	for _, b := range ctx.AllBlocks {
		findings = append(findings, rule.Check(b, ctx)...)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for trivial subflow, got %d", len(findings))
	}
}

func TestSubflowNoErrorHandlerRule_NonTrivialNoHandler_EmitsFinding(t *testing.T) {
	rule := &SubflowNoErrorHandlerRule{}
	abs := actionBlocks(4)
	sf := makeSubflow("sf1", "Main", abs...)
	flow := makeFlowWithSubflows(sf)
	ctx := buildContext(flow, nil)

	var findings []models.Finding
	for _, b := range ctx.AllBlocks {
		findings = append(findings, rule.Check(b, ctx)...)
	}
	if len(findings) != 1 {
		t.Fatalf("expected 1 finding, got %d", len(findings))
	}
	if findings[0].RuleID != "subflow-no-error-handler" {
		t.Errorf("ruleID = %q", findings[0].RuleID)
	}
	if findings[0].SubflowID != "sf1" {
		t.Errorf("SubflowID = %q, want sf1", findings[0].SubflowID)
	}
}

func TestSubflowNoErrorHandlerRule_HasTopLevelHandler_NoFinding(t *testing.T) {
	rule := &SubflowNoErrorHandlerRule{}
	abs := actionBlocks(4)
	handler := makeBlock("eh1", "OnError", models.BlockTypeErrorHandler, "OnBlockError", 0)
	sf := makeSubflow("sf1", "Main", append(abs, handler)...)
	flow := makeFlowWithSubflows(sf)
	ctx := buildContext(flow, nil)

	var findings []models.Finding
	for _, b := range ctx.AllBlocks {
		findings = append(findings, rule.Check(b, ctx)...)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings when error handler present, got %d", len(findings))
	}
}

func TestSubflowNoErrorHandlerRule_HasNestedHandler_NoFinding(t *testing.T) {
	// Error handler is nested inside a loop — should still suppress the finding.
	rule := &SubflowNoErrorHandlerRule{}
	loop := makeBlock("loop1", "Loop", models.BlockTypeLoop, "Loop.ForEach", 0)
	for i := 0; i < 4; i++ {
		child := *makeBlock(string(rune('a'+i)), "Act", models.BlockTypeAction, "SetVariable.Set", 4)
		child.SubflowID = "sf1"
		loop.Children = append(loop.Children, child)
	}
	handler := makeBlock("eh1", "OnError", models.BlockTypeErrorHandler, "OnBlockError", 4)
	loop.Children = append(loop.Children, *handler)
	sf := makeSubflow("sf1", "Main", loop)
	flow := makeFlowWithSubflows(sf)
	ctx := buildContext(flow, nil)

	var findings []models.Finding
	for _, b := range ctx.AllBlocks {
		findings = append(findings, rule.Check(b, ctx)...)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings when nested error handler present, got %d", len(findings))
	}
}

func TestSubflowNoErrorHandlerRule_OnlyOneFindingPerSubflow(t *testing.T) {
	// Even with many blocks, only one finding per subflow should be emitted.
	rule := &SubflowNoErrorHandlerRule{}
	abs := actionBlocks(8)
	sf := makeSubflow("sf1", "Main", abs...)
	flow := makeFlowWithSubflows(sf)
	ctx := buildContext(flow, nil)

	var findings []models.Finding
	for _, b := range ctx.AllBlocks {
		findings = append(findings, rule.Check(b, ctx)...)
	}
	if len(findings) != 1 {
		t.Errorf("expected exactly 1 finding, got %d", len(findings))
	}
}

func TestSubflowNoErrorHandlerRule_MultipleSubflows_IndependentFindings(t *testing.T) {
	// Two non-trivial subflows, neither with a handler → 2 findings.
	// Use non-overlapping ID offsets so blocks don't collide in ctx.AllBlocks.
	rule := &SubflowNoErrorHandlerRule{}
	sf1 := makeSubflow("sf1", "Main", actionBlocksAt(0, 4)...)
	sf2 := makeSubflow("sf2", "Helper", actionBlocksAt(10, 4)...)
	flow := makeFlowWithSubflows(sf1, sf2)
	ctx := buildContext(flow, nil)

	var findings []models.Finding
	for _, b := range ctx.AllBlocks {
		findings = append(findings, rule.Check(b, ctx)...)
	}
	if len(findings) != 2 {
		t.Errorf("expected 2 findings (one per subflow), got %d", len(findings))
	}
}

func TestSubflowNoErrorHandlerRule_CommentOnlySubflow_NoFinding(t *testing.T) {
	rule := &SubflowNoErrorHandlerRule{}
	comments := make([]*models.Block, 5)
	for i := range comments {
		comments[i] = makeBlock(string(rune('a'+i)), "Comment", models.BlockTypeComment, "", 0)
	}
	sf := makeSubflow("sf1", "Comments", comments...)
	flow := makeFlowWithSubflows(sf)
	ctx := buildContext(flow, nil)

	var findings []models.Finding
	for _, b := range ctx.AllBlocks {
		findings = append(findings, rule.Check(b, ctx)...)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for comment-only subflow, got %d", len(findings))
	}
}

func TestSubflowNoErrorHandlerRule_EmptySubflow_NoFinding(t *testing.T) {
	rule := &SubflowNoErrorHandlerRule{}
	sf := models.Subflow{ID: "sf1", Name: "Empty", Blocks: []models.Block{}}
	flow := makeFlowWithSubflows(sf)
	ctx := buildContext(flow, nil)

	var findings []models.Finding
	for _, b := range ctx.AllBlocks {
		findings = append(findings, rule.Check(b, ctx)...)
	}
	if len(findings) != 0 {
		t.Errorf("expected 0 findings for empty subflow, got %d", len(findings))
	}
}

// ---- Registered in AllRules -----------------------------------------------

func TestSubflowNoErrorHandlerRule_RegisteredInAllRules(t *testing.T) {
	for _, r := range AllRules() {
		if r.ID() == "subflow-no-error-handler" {
			return
		}
	}
	t.Error("subflow-no-error-handler not found in AllRules()")
}

// ---- DefaultSettings includes the new rule ---------------------------------

func TestDefaultSettings_ContainsAllRules(t *testing.T) {
	defaults := models.DefaultSettings()
	for _, r := range AllRules() {
		if _, ok := defaults.Analysis.Rules[r.ID()]; !ok {
			t.Errorf("rule %q is in AllRules() but missing from DefaultSettings", r.ID())
		}
	}
}
