package analyzer

import (
	"math"
	"strings"
	"testing"

	"pad-core/models"
)

// ---- Rule identity methods --------------------------------------------------
// AllRules() covers ID(); this test covers Name/Description/DefaultSeverity/Category.

func TestAllRules_IdentityMethods(t *testing.T) {
	for _, r := range AllRules() {
		if r.Name() == "" {
			t.Errorf("rule %q has empty Name()", r.ID())
		}
		if r.Description() == "" {
			t.Errorf("rule %q has empty Description()", r.ID())
		}
		if r.Category() == "" {
			t.Errorf("rule %q has empty Category()", r.ID())
		}
		sev := r.DefaultSeverity()
		if sev != models.SeverityError && sev != models.SeverityWarning && sev != models.SeverityInfo {
			t.Errorf("rule %q has unexpected DefaultSeverity %q", r.ID(), sev)
		}
	}
}

// ---- GetParent --------------------------------------------------------------

func TestGetParent_Found(t *testing.T) {
	parent := makeBlock("p", "Parent", models.BlockTypeLoop, "Loop.ForEach", 0)
	child := makeBlock("c", "Child", models.BlockTypeAction, "SetVariable.Set", 4)
	parent.SubflowID = "sf1"
	child.SubflowID = "sf1"
	parent.Children = []models.Block{*child}

	flow := &models.FlowDocument{
		ID:       "test",
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*parent}}},
	}
	ctx := buildContext(flow, nil)

	got := GetParent(ctx, ctx.AllBlocks["c"])
	if got == nil {
		t.Fatal("expected non-nil parent")
	}
	if got.ID != "p" {
		t.Errorf("parent ID = %q, want %q", got.ID, "p")
	}
}

func TestGetParent_TopLevel_ReturnsNil(t *testing.T) {
	b := makeBlock("b1", "Top", models.BlockTypeAction, "SetVariable.Set", 0)
	b.SubflowID = "sf1"
	flow := &models.FlowDocument{
		ID:       "test",
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}},
	}
	ctx := buildContext(flow, nil)

	got := GetParent(ctx, ctx.AllBlocks["b1"])
	if got != nil {
		t.Errorf("expected nil parent for top-level block, got %+v", got)
	}
}

// ---- HasErrorHandlerAncestor ------------------------------------------------

func TestHasErrorHandlerAncestor_DirectParentIsHandler(t *testing.T) {
	action := makeBlock("a1", "Risky", models.BlockTypeAction, "WebAutomation.Click", 4)
	action.SubflowID = "sf1"
	handler := makeBlock("h1", "Handler", models.BlockTypeErrorHandler, "ON_ERROR", 0)
	handler.SubflowID = "sf1"
	handler.Children = []models.Block{*action}

	flow := &models.FlowDocument{
		ID:       "test",
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*handler}}},
	}
	ctx := buildContext(flow, nil)

	if !HasErrorHandlerAncestor(ctx, ctx.AllBlocks["a1"]) {
		t.Error("expected true when direct parent is an error handler")
	}
}

func TestHasErrorHandlerAncestor_SiblingIsHandler(t *testing.T) {
	// Pattern: action and handler are children of the same parent block.
	// HasErrorHandlerAncestor checks SiblingMap[parentID], so siblings must share a parent.
	action := makeBlock("a1", "Action", models.BlockTypeAction, "WebAutomation.Click", 4)
	action.SubflowID = "sf1"
	handler := makeBlock("h1", "Handler", models.BlockTypeErrorHandler, "ON_ERROR", 4)
	handler.SubflowID = "sf1"
	parent := makeBlock("p1", "Parent", models.BlockTypeLoop, "Loop.ForEach", 0)
	parent.SubflowID = "sf1"
	parent.Children = []models.Block{*action, *handler}

	flow := &models.FlowDocument{
		ID:       "test",
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*parent}}},
	}
	ctx := buildContext(flow, nil)

	if !HasErrorHandlerAncestor(ctx, ctx.AllBlocks["a1"]) {
		t.Error("expected true when a sibling (under common parent) is an error handler")
	}
}

func TestHasErrorHandlerAncestor_NoHandler(t *testing.T) {
	action := makeBlock("a1", "Action", models.BlockTypeAction, "SetVariable.Set", 0)
	action.SubflowID = "sf1"
	other := makeBlock("a2", "Other", models.BlockTypeAction, "SetVariable.Set", 0)
	other.SubflowID = "sf1"

	flow := &models.FlowDocument{
		ID:       "test",
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*action, *other}}},
	}
	ctx := buildContext(flow, nil)

	if HasErrorHandlerAncestor(ctx, ctx.AllBlocks["a1"]) {
		t.Error("expected false when no error handler is present")
	}
}

// ---- blockReferencesVar -----------------------------------------------------

func TestBlockReferencesVar_ViaVariablesList(t *testing.T) {
	b := makeBlock("b1", "Close", models.BlockTypeAction, "File.CloseTextFile", 0)
	b.Variables = []string{"MyFile"}
	if !blockReferencesVar(b, "MyFile") {
		t.Error("expected true when var is in Variables list")
	}
}

func TestBlockReferencesVar_ViaPropertyPlain(t *testing.T) {
	b := makeBlock("b1", "Close", models.BlockTypeAction, "File.CloseTextFile", 0)
	b.Properties = map[string]string{"FileHandle": "MyFile"}
	if !blockReferencesVar(b, "MyFile") {
		t.Error("expected true when property value equals varName")
	}
}

func TestBlockReferencesVar_ViaPropertyWrapped(t *testing.T) {
	b := makeBlock("b1", "Action", models.BlockTypeAction, "SetVariable.Set", 0)
	b.Properties = map[string]string{"Value": "%MyFile%"}
	if !blockReferencesVar(b, "MyFile") {
		t.Error("expected true when property value equals wrapped varName")
	}
}

func TestBlockReferencesVar_NotReferenced(t *testing.T) {
	b := makeBlock("b1", "Action", models.BlockTypeAction, "SetVariable.Set", 0)
	b.Variables = []string{"OtherVar"}
	b.Properties = map[string]string{"Key": "something_else"}
	if blockReferencesVar(b, "MyFile") {
		t.Error("expected false when var is not referenced")
	}
}

// ---- isHighEntropySecret / isAlphanumeric / shannonEntropy ------------------

func TestIsAlphanumeric_AllAlnum(t *testing.T) {
	if !isAlphanumeric("abc123XYZ") {
		t.Error("expected true for alphanumeric string")
	}
}

func TestIsAlphanumeric_WithSymbol(t *testing.T) {
	if isAlphanumeric("abc-123") {
		t.Error("expected false for string with hyphen")
	}
}

func TestIsAlphanumeric_Empty(t *testing.T) {
	if !isAlphanumeric("") {
		t.Error("expected true for empty string (vacuously true)")
	}
}

func TestShannonEntropy_Empty(t *testing.T) {
	if got := shannonEntropy(""); got != 0 {
		t.Errorf("shannonEntropy(\"\") = %v, want 0", got)
	}
}

func TestShannonEntropy_SingleChar(t *testing.T) {
	// Single unique character → entropy = 0 (only one symbol).
	got := shannonEntropy("aaaa")
	if got != 0 {
		t.Errorf("shannonEntropy(\"aaaa\") = %v, want 0", got)
	}
}

func TestShannonEntropy_MaxEntropy(t *testing.T) {
	// All distinct characters → high entropy (log2(n) where n = len(s)).
	s := "abcdefgh" // 8 distinct chars, each appears once → log2(8) = 3
	got := shannonEntropy(s)
	want := math.Log2(8)
	if math.Abs(got-want) > 0.001 {
		t.Errorf("shannonEntropy(%q) = %v, want ~%v", s, got, want)
	}
}

func TestIsHighEntropySecret_TooShort(t *testing.T) {
	if isHighEntropySecret("abc123") {
		t.Error("expected false for short string (< 48 chars)")
	}
}

func TestIsHighEntropySecret_NonAlphanumeric(t *testing.T) {
	// ≥48 chars but contains a dash → not alphanumeric → false.
	s := "abcdefghij-klmnopqrstuvwxyz0123456789ABCDEFGHIJK"
	if isHighEntropySecret(s) {
		t.Error("expected false for string with non-alphanumeric characters")
	}
}

func TestIsHighEntropySecret_LowEntropy(t *testing.T) {
	// ≥48 chars, alphanumeric, but all the same character → entropy ≈ 0.
	s := strings.Repeat("a", 50)
	if isHighEntropySecret(s) {
		t.Error("expected false for low-entropy string")
	}
}

func TestIsHighEntropySecret_HighEntropy(t *testing.T) {
	// ≥48 chars, all distinct alphanumeric → entropy log2(48) ≈ 5.58 > 5.0.
	s := "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUV"
	if !isHighEntropySecret(s) {
		t.Error("expected true for long, high-entropy alphanumeric string")
	}
}

func TestIsHighEntropySecret_HexDigestNotFlagged(t *testing.T) {
	// A SHA-256 hex digest (64 chars over a 16-symbol alphabet, entropy ≤ 4.0)
	// is a common non-secret literal and must not be flagged.
	s := "9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08"
	if isHighEntropySecret(s) {
		t.Error("expected false for hex digest")
	}
}

func TestIsHighEntropySecret_ShortMixedCaseIDNotFlagged(t *testing.T) {
	// Mixed-case Base62 identifiers in the 32-47 char range (record/file IDs)
	// were the main false-positive source before the 48-char floor.
	s := "aB3dE7fG9hJ2kL5mN8pQrS4tUvW6xYz0Ab" // 34 chars
	if isHighEntropySecret(s) {
		t.Error("expected false for sub-48-char identifier")
	}
}

// ---- DuplicateActionRule: not-first-in-run returns nil ---------------------

func TestDuplicateActionRule_NotFirstInRun_ReturnsNil(t *testing.T) {
	rule := &DuplicateActionRule{}
	props := map[string]string{"Target": "btn1"}
	b1 := makeBlock("b1", "Click", models.BlockTypeAction, "WebAutomation.Click", 0)
	b1.Properties = props
	b1.SubflowID = "sf1"
	b2 := makeBlock("b2", "Click", models.BlockTypeAction, "WebAutomation.Click", 0)
	b2.Properties = props
	b2.SubflowID = "sf1"
	b3 := makeBlock("b3", "Click", models.BlockTypeAction, "WebAutomation.Click", 0)
	b3.Properties = props
	b3.SubflowID = "sf1"

	flow := &models.FlowDocument{
		ID:       "test",
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b1, *b2, *b3}}},
	}
	ctx := buildContext(flow, nil)

	// b2 is in the middle of the run → not the first → should return nil.
	if got := rule.Check(ctx.AllBlocks["b2"], ctx); len(got) != 0 {
		t.Errorf("expected nil for non-first block in duplicate run, got %d findings", len(got))
	}
	// b3 is last → not the first → should return nil.
	if got := rule.Check(ctx.AllBlocks["b3"], ctx); len(got) != 0 {
		t.Errorf("expected nil for last block in duplicate run, got %d findings", len(got))
	}
}

// ---- MissingDelayRule: first block and self-is-wait edge cases -------------

func TestMissingDelayRule_FirstBlock_ReturnsNil(t *testing.T) {
	rule := &MissingDelayRule{}
	b1 := makeBlock("b1", "First click", models.BlockTypeAction, "WebAutomation.Click", 0)
	b1.SubflowID = "sf1"
	b2 := makeBlock("b2", "Second click", models.BlockTypeAction, "WebAutomation.Click", 0)
	b2.SubflowID = "sf1"
	flow := &models.FlowDocument{
		ID:       "test",
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b1, *b2}}},
	}
	ctx := buildContext(flow, nil)
	// b1 is at myIdx==0 → myIdx <= 0 guard → nil.
	if got := rule.Check(ctx.AllBlocks["b1"], ctx); len(got) != 0 {
		t.Errorf("expected nil for first block, got %d findings", len(got))
	}
}

func TestMissingDelayRule_SelfIsWait_ReturnsNil(t *testing.T) {
	rule := &MissingDelayRule{}
	b1 := makeBlock("b1", "Click", models.BlockTypeAction, "WebAutomation.Click", 0)
	b1.SubflowID = "sf1"
	wait := makeBlock("b2", "Wait element", models.BlockTypeAction, "WebAutomation.WaitForElement", 0)
	wait.SubflowID = "sf1"
	flow := &models.FlowDocument{
		ID:       "test",
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b1, *wait}}},
	}
	ctx := buildContext(flow, nil)
	// wait is itself a wait action → isWaitAction guard → nil.
	if got := rule.Check(ctx.AllBlocks["b2"], ctx); len(got) != 0 {
		t.Errorf("expected nil when block is itself a wait action, got %d findings", len(got))
	}
}

// ---- UnusedVariableRule: _var fallback and no _output/_var -----------------

func TestUnusedVariableRule_VarPropertyFallback(t *testing.T) {
	rule := &UnusedVariableRule{}
	// Uses "_var" instead of "_output".
	b := makeBlock("b1", "Set MyVar", models.BlockTypeVariable, "SET", 0)
	b.Properties = map[string]string{"_var": "MyVar"}
	b.SubflowID = "sf1"
	flow := &models.FlowDocument{
		ID:       "test",
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}},
	}
	ctx := buildContext(flow, nil)
	got := rule.Check(b, ctx)
	if len(got) != 1 {
		t.Fatalf("expected 1 finding when var declared via _var but unused, got %d", len(got))
	}
}

func TestUnusedVariableRule_NoOutputOrVar_ReturnsNil(t *testing.T) {
	rule := &UnusedVariableRule{}
	b := makeBlock("b1", "Set", models.BlockTypeVariable, "SET", 0)
	b.Properties = map[string]string{} // neither _output nor _var
	b.SubflowID = "sf1"
	flow := &models.FlowDocument{
		ID:       "test",
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}},
	}
	ctx := buildContext(flow, nil)
	if got := rule.Check(b, ctx); len(got) != 0 {
		t.Errorf("expected nil when no declared var name, got %d findings", len(got))
	}
}
