package analyzer

import (
	"sort"
	"testing"

	"pad-core/models"
)

// expectedRuleIDs is the authoritative catalog of shipped rules.
//
// Rules self-register from each rule file's init(), which makes ADDING one
// frictionless but makes LOSING one silent: registry_test.go asserts only that
// the list is non-empty, that every rule has an ID and Name, and that IDs do
// not collide. Delete a rule file, or drop its init(), and all three still
// pass. A mutation sweep found 10 rules that could be deregistered with a fully
// green suite, path-traversal-risk (Security) among them.
//
// So this list is a deliberate, reviewed inventory: removing or renaming a rule
// is a two-place edit, and the diff shows a reviewer exactly which detection
// the product gained or lost. Adding a rule means adding its ID here.
var expectedRuleIDs = []string{
	"circular-subflow-dependency",
	"command-injection-risk",
	"dead-code",
	"dead-data",
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

// expectedRuleAutoFix is the authoritative rule -> fixer wiring.
//
// A finding is only offerable as a fix when engine.go's ruleAutoFix table maps
// its rule ID to a fixer. The round-trip fix gates call the fixer FUNCTIONS
// directly (e.g. MaskSensitiveVariablePatch(...)), so they stay green whether
// or not any finding can actually reach that fixer — renaming a key so the
// lookup misses was survivable for 22 of these, including every security
// fixer: sensitive-exposure, sql-injection-risk, insecure-http-url and the
// hardcoded-* family. This table is what makes those wirings load-bearing.
var expectedRuleAutoFix = map[string]string{
	"command-injection-risk":   "sanitize-command-vars",
	"dead-code":                "remove-block",
	"dead-data":                "remove-block",
	"disabled-block":           "remove-block",
	"duplicate-action":         "remove-block",
	"empty-branch":             "remove-block",
	"empty-handler":            "insert-handler-log",
	"error-swallow":            "insert-error-log",
	"file-op-no-error-handler": "wrap-error-handler",
	"hardcoded-credential":     "replace-with-variable",
	"hardcoded-filepath":       "replace-with-variable",
	"hardcoded-ip":             "replace-with-variable",
	"hardcoded-ui-coordinates": "replace-with-variable",
	"hardcoded-url":            "replace-with-variable",
	"infinite-loop-risk":       "insert-exit-condition",
	"insecure-http-url":        "upgrade-to-https",
	"missing-delay":            "insert-delay",
	"missing-retry":            "wrap-in-retry",
	"missing-timeout":          "set-timeout",
	"redundant-action":         "remove-block",
	"resource-leak":            "insert-close",
	"sensitive-exposure":       "mask-sensitive-variable",
	"slow-pattern":             "insert-delay-in-loop",
	"sql-injection-risk":       "parameterize-sql",
	"subflow-no-error-handler": "wrap-error-handler",
	"switch-no-default":        "insert-default",
	"todo-in-comment":          "remove-block",
	"unhandled-error":          "wrap-error-handler",
	"uninitialized-variable":   "init-variable",
	"unused-variable":          "remove-block",
	"wait-zero":                "remove-block",
}

// TestRegistry_ExactRuleSet pins the shipped catalog. It fails in BOTH
// directions on purpose: a lost rule is a silent loss of detection, and an
// unlisted new rule is a reminder to record it here.
func TestRegistry_ExactRuleSet(t *testing.T) {
	var got []string
	for _, r := range AllRules() {
		got = append(got, r.ID())
	}
	sort.Strings(got)

	want := append([]string(nil), expectedRuleIDs...)
	sort.Strings(want)

	inWant := map[string]bool{}
	for _, id := range want {
		inWant[id] = true
	}
	inGot := map[string]bool{}
	for _, id := range got {
		inGot[id] = true
	}

	for _, id := range want {
		if !inGot[id] {
			t.Errorf("rule %q is no longer registered — the product silently lost this detection", id)
		}
	}
	for _, id := range got {
		if !inWant[id] {
			t.Errorf("rule %q is registered but not listed in expectedRuleIDs — add it, so the "+
				"catalog stays a reviewed inventory", id)
		}
	}
	if len(got) != len(want) {
		t.Errorf("rule count %d, want %d", len(got), len(want))
	}
}

// TestRegistry_ExactAutoFixWiring pins which rules can offer a fix. Losing a
// wiring does not fail any existing test: the finding simply stops being
// fixable, and every fixer unit test keeps passing because it calls the patch
// function directly.
func TestRegistry_ExactAutoFixWiring(t *testing.T) {
	for ruleID, fixType := range expectedRuleAutoFix {
		got, ok := ruleAutoFix[ruleID]
		if !ok {
			t.Errorf("rule %q lost its auto-fix wiring (want %q) — findings from it are no longer "+
				"offerable as a fix, and no fixer unit test notices", ruleID, fixType)
			continue
		}
		if got != fixType {
			t.Errorf("rule %q is wired to fixer %q, want %q", ruleID, got, fixType)
		}
	}
	for ruleID, fixType := range ruleAutoFix {
		if _, ok := expectedRuleAutoFix[ruleID]; !ok {
			t.Errorf("rule %q is wired to fixer %q but is not in expectedRuleAutoFix — add it", ruleID, fixType)
		}
	}
}

// TestRegistry_EveryWiredRuleExists catches the other half of a rename: a
// ruleAutoFix key that matches no registered rule is dead weight that looks
// like working coverage.
func TestRegistry_EveryWiredRuleExists(t *testing.T) {
	registered := map[string]bool{}
	for _, r := range AllRules() {
		registered[r.ID()] = true
	}
	for ruleID := range ruleAutoFix {
		if !registered[ruleID] {
			t.Errorf("ruleAutoFix maps %q, which is not a registered rule — the wiring can never "+
				"fire, so the fix it names is unreachable", ruleID)
		}
	}
}

// TestRegistry_EveryWiredFixerIsDispatchable closes the last gap: a wiring that
// names a fixer the dispatcher does not implement produces a finding that
// advertises a fix which then does nothing. PatchForFix is the single source of
// truth for that mapping and already errors on an unknown type, so this asks it
// directly rather than duplicating the list.
func TestRegistry_EveryWiredFixerIsDispatchable(t *testing.T) {
	block := &models.Block{
		ID:         "b1",
		RawType:    "HTTPClient.InvokeUrl",
		Name:       "Invoke Url",
		LineNumber: 1,
		Properties: map[string]string{"Url": "https://example.com"},
		Variables:  []string{},
	}
	for ruleID, fixType := range ruleAutoFix {
		if _, err := PatchForFix(block, fixType, ruleID, "v", "Url"); err != nil {
			t.Errorf("rule %q advertises fixer %q, which the dispatcher rejects: %v",
				ruleID, fixType, err)
		}
	}
}
