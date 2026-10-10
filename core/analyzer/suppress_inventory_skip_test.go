package analyzer

import (
	"sync/atomic"
	"testing"

	"pad-core/models"
)

// countingRule counts Check calls so tests can assert how much rule work ran.
type countingRule struct{ calls *atomic.Int64 }

func (countingRule) ID() string                       { return "test-counting" }
func (countingRule) Name() string                     { return "Test counting" }
func (countingRule) Description() string              { return "" }
func (countingRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (countingRule) Category() string                 { return "test" }
func (r countingRule) Check(*models.Block, *RuleContext) []models.Finding {
	r.calls.Add(1)
	return nil
}

// TestSuppressionInventory_NoDirectiveRunsNoRules: the inventory re-ran every
// rule over every block single-threaded even when the flow had no pad-ignore
// directive at all (slower than a cold parallel analysis) only to return
// nothing. With no directive it must return nil without running any rule.
func TestSuppressionInventory_NoDirectiveRunsNoRules(t *testing.T) {
	var calls atomic.Int64
	rules := append(AllRules(), countingRule{&calls})

	// A plain comment and a nested block are not directives.
	flow := buildLargeFlow(200)
	flow.Subflows[0].Blocks = append(flow.Subflows[0].Blocks,
		*makeBlock("c-plain", "just a note about pad ignoring", models.BlockTypeComment, "COMMENT", 0))
	if got := SuppressionInventory(flow, rules, nil); got != nil {
		t.Fatalf("no directive: got %+v, want nil", got)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("no directive: inventory ran %d rule checks, want 0", n)
	}
	if got := newTestAnalyzer().SuppressionInventory(localIn(flow, rules, nil, nil)); got != nil {
		t.Fatalf("no directive (cached): got %+v, want nil", got)
	}
	if n := calls.Load(); n != 0 {
		t.Fatalf("no directive (cached): inventory ran %d rule checks, want 0", n)
	}

	// Sanity: a directive still triggers the full audit.
	flow.Subflows[0].Blocks = append(flow.Subflows[0].Blocks,
		*makeBlock("c-dir", "pad-ignore[unhandled-error]", models.BlockTypeComment, "COMMENT", 0))
	if got := SuppressionInventory(flow, rules, nil); len(got) != 1 {
		t.Fatalf("with a directive: got %+v, want one entry", got)
	}
	if calls.Load() == 0 {
		t.Fatal("with a directive: no rule ran")
	}
}

// TestHasPadIgnoreDirective_MatchesInventoryReach: the pre-check must find
// every directive the inventory walk lists — nested inside a block's children
// included — or the early return would hide real entries.
func TestHasPadIgnoreDirective_MatchesInventoryReach(t *testing.T) {
	loop := makeBlock("loop", "LOOP i FROM 1 TO 3", models.BlockTypeLoop, "LOOP", 0)
	loop.Children = []models.Block{
		*makeBlock("c-nested", "pad-ignore", models.BlockTypeComment, "COMMENT", 4),
		*makeBlock("act", "Click", models.BlockTypeAction, "WebAutomation.Click", 4),
	}
	flow := &models.FlowDocument{ID: "nested", Subflows: []models.Subflow{
		{ID: "sf0", Name: "Empty"},
		{ID: "sf1", Name: "Main", Blocks: []models.Block{*loop}},
	}}
	if !hasPadIgnoreDirective(flow) {
		t.Fatal("nested directive not detected")
	}
	got := SuppressionInventory(flow, AllRules(), nil)
	if len(got) != 1 || got[0].Rule != "*" {
		t.Fatalf("nested directive: got %+v", got)
	}
}
