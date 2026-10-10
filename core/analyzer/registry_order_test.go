package analyzer

import (
	"slices"
	"testing"
)

// pinnedRuleOrder is AllRules() in registration order: init order, i.e.
// dataflow.go first, then the rule_*.go files by file name. Findings are
// aggregated rule-major, so this order decides finding order, the F-nnn IDs,
// the fix loop's pick order and the order of every export. Renaming a rule
// file or registering a rule elsewhere changes it; registry_pin_test.go pins
// only the sorted set.
var pinnedRuleOrder = []string{
	"dead-data",
	"circular-subflow-dependency",
	"command-injection-risk",
	"dead-code",
	"deep-nesting",
	"disabled-block",
	"duplicate-action",
	"duplicate-label",
	"duplicate-subflow-name",
	"empty-branch",
	"empty-handler",
	"empty-subflow",
	"error-swallow",
	"file-op-no-error-handler",
	"goto-antipattern",
	"hardcoded-credential",
	"hardcoded-filepath",
	"hardcoded-ip",
	"hardcoded-ui-coordinates",
	"hardcoded-url",
	"high-cyclomatic-complexity",
	"infinite-loop-risk",
	"insecure-http-url",
	"large-subflow",
	"missing-delay",
	"missing-retry",
	"missing-timeout",
	"parse-error",
	"path-traversal-risk",
	"redundant-action",
	"resource-leak",
	"sensitive-exposure",
	"slow-pattern",
	"sql-injection-risk",
	"subflow-mismatch",
	"subflow-no-error-handler",
	"switch-no-default",
	"tainted-sink",
	"todo-in-comment",
	"uncalled-subflow",
	"unhandled-error",
	"uninitialized-variable",
	"unused-variable",
	"wait-zero",
	"wide-loop",
}

func TestAllRules_OrderPinned(t *testing.T) {
	var got []string
	for _, r := range AllRules() {
		got = append(got, r.ID())
	}
	if slices.Equal(got, pinnedRuleOrder) {
		return
	}
	for i := 0; i < len(got) || i < len(pinnedRuleOrder); i++ {
		var g, w string
		if i < len(got) {
			g = got[i]
		}
		if i < len(pinnedRuleOrder) {
			w = pinnedRuleOrder[i]
		}
		if g != w {
			t.Fatalf("AllRules() order changed at position %d: got %q, want %q (finding order and F-nnn IDs follow it)\ngot:  %v",
				i, g, w, got)
		}
	}
}
