package ai

import (
	"fmt"
	"strings"
	"testing"

	"pad-core/models"
)

// The Variable History section lists its variables by name and shows at most maxVariableHistoryEvents events
// of each, saying how many there are: the section is read in every turn about the block, so it is bounded and
// stable (R-analyzer-boundary-4).
func TestBuildContext_VariableHistory_SortedAndBounded(t *testing.T) {
	block := &models.Block{ID: "b1", Name: "Set total", Type: models.BlockTypeAction, RawType: "Variables.SetVariable", SubflowID: "sf1"}
	doc := makeMinimalDoc("Hist")
	doc.BlocksByID["b1"] = block
	long := make([]models.VariableEvent, 0, 15)
	for i := 0; i < 15; i++ {
		long = append(long, models.VariableEvent{Type: "read", BlockID: fmt.Sprintf("r%d", i), Line: 10 + i})
	}
	req := ContextRequest{
		Flow: doc, SelectedBlock: block, TokenBudget: 8000,
		VariableEvents: map[string][]models.VariableEvent{
			"Zeta":  {{Type: "init", BlockID: "z", Line: 1}},
			"Alpha": {{Type: "init", BlockID: "a", Line: 2}},
			"Total": long,
		},
	}
	_, ctx := BuildContext(req)
	at := strings.Index(ctx, "## Variable History")
	if at < 0 {
		t.Fatalf("no Variable History section:\n%s", ctx)
	}
	section := ctx[at:]
	alpha, total, zeta := strings.Index(section, "**%Alpha%**"), strings.Index(section, "**%Total%**"), strings.Index(section, "**%Zeta%**")
	if alpha < 0 || total < 0 || zeta < 0 || alpha >= total || total >= zeta {
		t.Errorf("variables are not listed by name (Alpha %d, Total %d, Zeta %d):\n%s", alpha, total, zeta, section)
	}
	if !strings.Contains(section, fmt.Sprintf("**%%Total%%** (15 events, first %d shown", maxVariableHistoryEvents)) {
		t.Errorf("the bounded variable does not say how many events it has:\n%s", section)
	}
	if n := strings.Count(section, "[L1"); n != maxVariableHistoryEvents+1 { // L10..L19 of Total, plus L1 of Zeta
		t.Errorf("Total shows %d events, want %d:\n%s", n-1, maxVariableHistoryEvents, section)
	}
}
