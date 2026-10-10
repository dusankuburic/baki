package analyzer

import (
	"slices"
	"testing"

	"pad-core/models"
)

// bothKinds implements BlockRule and FlowRule; the engine dispatches it as a
// flow rule, so the catalog must name it one.
type bothKinds struct{ identityRule }

func (bothKinds) Check(*models.Block, *RuleContext) []models.Finding { return nil }
func (bothKinds) CheckFlow(*RuleContext) []models.Finding            { return nil }

// TestRuleKind_NamesWhatTheEngineDispatches: the catalog's kind agrees with
// kindOf for every built-in, names the library rules, and covers custom rules
// and rules of no kind.
func TestRuleKind_NamesWhatTheEngineDispatches(t *testing.T) {
	var flow []string
	for _, r := range AllRules() {
		switch got := RuleKind(r); got {
		case RuleKindFlow:
			flow = append(flow, r.ID())
		case RuleKindBlock:
		default:
			t.Errorf("%s: RuleKind = %q, want block or flow", r.ID(), got)
		}
	}
	slices.Sort(flow)
	if !slices.Equal(flow, pinnedFlowRules) {
		t.Errorf("flow kinds = %v, want %v", flow, pinnedFlowRules)
	}

	if len(LibraryRules()) == 0 {
		t.Fatal("no library rule registered")
	}
	for _, r := range LibraryRules() {
		if got := RuleKind(r); got != RuleKindLibrary {
			t.Errorf("%s: RuleKind = %q, want %q", r.ID(), got, RuleKindLibrary)
		}
	}

	custom, err := NewCustomRule(CustomRuleConfig{ID: "org-kind", Name: "Kind", Severity: "info", NameMatch: "x"})
	if err != nil {
		t.Fatalf("NewCustomRule: %v", err)
	}
	for _, c := range []struct {
		r    Rule
		want string
	}{
		{custom, RuleKindBlock},
		{bothKinds{identityRule{"both"}}, RuleKindFlow},
		{identityRule{"none"}, ""},
	} {
		if got := RuleKind(c.r); got != c.want {
			t.Errorf("RuleKind(%T) = %q, want %q", c.r, got, c.want)
		}
	}
}
