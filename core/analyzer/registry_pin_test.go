package analyzer

import (
	"sort"
	"testing"

	"pad-core/models"
)

// expectedRules is the authoritative catalog of shipped rules, with the
// metadata a recorded rule set compares against (ADR 0014): a rule is "new"
// to a baseline when it was introduced after it, revised, or promoted.
//
// Rules self-register from each rule file's init(), which makes ADDING one
// frictionless but makes LOSING one silent: registry_test.go (since deleted)
// asserted only that the list is non-empty, that every rule has an ID and
// Name, and that IDs do not collide. Delete a rule file, or drop its init(),
// and all three still passed. A mutation sweep found 10 rules that could be
// deregistered with a fully green suite, path-traversal-risk (Security) among
// them.
//
// So this list is a deliberate, reviewed inventory: removing or renaming a
// rule, shipping a new one, bumping a Revision or promoting a preview rule is a
// two-place edit, and the diff shows a reviewer exactly what changed. Revision
// 2 marks the rules whose findings Phase 4a's determinism fixes changed.
type expectedRule struct {
	introducedIn, revision int
	stage                  Stage
}

var expectedRules = map[string]expectedRule{
	"circular-subflow-dependency": {introducedIn: 1, revision: 2, stage: StageStable},
	"command-injection-risk":      {introducedIn: 1, revision: 1, stage: StageStable},
	"dead-code":                   {introducedIn: 1, revision: 1, stage: StageStable},
	"dead-data":                   {introducedIn: 1, revision: 1, stage: StageStable},
	"deep-nesting":                {introducedIn: 1, revision: 1, stage: StageStable},
	"disabled-block":              {introducedIn: 1, revision: 1, stage: StageStable},
	"duplicate-action":            {introducedIn: 1, revision: 1, stage: StageStable},
	"duplicate-label":             {introducedIn: 1, revision: 1, stage: StageStable},
	"duplicate-subflow-name":      {introducedIn: 1, revision: 1, stage: StageStable},
	"empty-branch":                {introducedIn: 1, revision: 1, stage: StageStable},
	"empty-handler":               {introducedIn: 1, revision: 1, stage: StageStable},
	"empty-subflow":               {introducedIn: 1, revision: 1, stage: StageStable},
	"error-swallow":               {introducedIn: 1, revision: 1, stage: StageStable},
	"file-op-no-error-handler":    {introducedIn: 1, revision: 1, stage: StageStable},
	"goto-antipattern":            {introducedIn: 1, revision: 1, stage: StageStable},
	"hardcoded-credential":        {introducedIn: 1, revision: 1, stage: StageStable},
	"hardcoded-filepath":          {introducedIn: 1, revision: 2, stage: StageStable},
	"hardcoded-ip":                {introducedIn: 1, revision: 2, stage: StageStable},
	"hardcoded-ui-coordinates":    {introducedIn: 1, revision: 2, stage: StageStable},
	"hardcoded-url":               {introducedIn: 1, revision: 2, stage: StageStable},
	"high-cyclomatic-complexity":  {introducedIn: 1, revision: 1, stage: StageStable},
	"infinite-loop-risk":          {introducedIn: 1, revision: 1, stage: StageStable},
	"insecure-http-url":           {introducedIn: 1, revision: 2, stage: StageStable},
	"large-subflow":               {introducedIn: 1, revision: 1, stage: StageStable},
	"missing-delay":               {introducedIn: 1, revision: 1, stage: StageStable},
	"missing-retry":               {introducedIn: 1, revision: 1, stage: StageStable},
	"missing-timeout":             {introducedIn: 1, revision: 1, stage: StageStable},
	"parse-error":                 {introducedIn: 1, revision: 1, stage: StageStable},
	"path-traversal-risk":         {introducedIn: 1, revision: 2, stage: StageStable},
	"redundant-action":            {introducedIn: 1, revision: 1, stage: StageStable},
	"resource-leak":               {introducedIn: 1, revision: 1, stage: StageStable},
	"sensitive-exposure":          {introducedIn: 1, revision: 1, stage: StageStable},
	"slow-pattern":                {introducedIn: 1, revision: 1, stage: StageStable},
	"sql-injection-risk":          {introducedIn: 1, revision: 2, stage: StageStable},
	"subflow-mismatch":            {introducedIn: 1, revision: 1, stage: StageStable},
	"subflow-no-error-handler":    {introducedIn: 1, revision: 1, stage: StageStable},
	"switch-no-default":           {introducedIn: 1, revision: 1, stage: StageStable},
	"tainted-sink":                {introducedIn: 1, revision: 1, stage: StageStable},
	"todo-in-comment":             {introducedIn: 1, revision: 1, stage: StageStable},
	"uncalled-subflow":            {introducedIn: 1, revision: 1, stage: StageStable},
	"unhandled-error":             {introducedIn: 1, revision: 1, stage: StageStable},
	"uninitialized-variable":      {introducedIn: 1, revision: 1, stage: StageStable},
	"unused-variable":             {introducedIn: 1, revision: 1, stage: StageStable},
	"wait-zero":                   {introducedIn: 1, revision: 1, stage: StageStable},
	"wide-loop":                   {introducedIn: 1, revision: 1, stage: StageStable},
}

// expectedRuleAutoFix is the authoritative rule -> fixer wiring.
//
// A finding is only offerable as a fix when its rule's RuleMeta.AutoFix names
// a fixer. The round-trip fix gates call the fixer FUNCTIONS
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

// TestRegistry_ExactRuleSet pins the shipped catalog and each rule's
// IntroducedIn, Revision and Stage. It fails in BOTH directions on purpose: a
// lost rule is a silent loss of detection, and an unlisted new rule is a
// reminder to record it here.
func TestRegistry_ExactRuleSet(t *testing.T) {
	var got []string
	for _, r := range AllRules() {
		got = append(got, r.ID())
	}
	sort.Strings(got)

	inGot := map[string]bool{}
	for _, id := range got {
		inGot[id] = true
	}
	for id := range expectedRules {
		if !inGot[id] {
			t.Errorf("rule %q is no longer registered — the product silently lost this detection", id)
		}
	}
	for _, id := range got {
		want, ok := expectedRules[id]
		if !ok {
			t.Errorf("rule %q is registered but not listed in expectedRules — add it, so the "+
				"catalog stays a reviewed inventory", id)
			continue
		}
		m, _ := LookupMeta(id)
		gotRev := m.Revision
		if gotRev == 0 {
			gotRev = 1
		}
		if m.IntroducedIn != want.introducedIn || gotRev != want.revision || m.Stage != want.stage {
			t.Errorf("rule %q: IntroducedIn %d, Revision %d, Stage %q; pinned %d, %d, %q",
				id, m.IntroducedIn, gotRev, m.Stage, want.introducedIn, want.revision, want.stage)
		}
	}
	if len(got) != len(expectedRules) {
		t.Errorf("rule count %d, want %d", len(got), len(expectedRules))
	}
}

// TestRegistry_ExactAutoFixWiring pins which rules can offer a fix. Losing a
// wiring does not fail any existing test: the finding simply stops being
// fixable, and every fixer unit test keeps passing because it calls the patch
// function directly.
func TestRegistry_ExactAutoFixWiring(t *testing.T) {
	for ruleID, fixType := range expectedRuleAutoFix {
		got := RuleAutoFix(ruleID)
		if got == "" {
			t.Errorf("rule %q lost its auto-fix wiring (want %q) — findings from it are no longer "+
				"offerable as a fix, and no fixer unit test notices", ruleID, fixType)
			continue
		}
		if got != fixType {
			t.Errorf("rule %q is wired to fixer %q, want %q", ruleID, got, fixType)
		}
	}
	for _, r := range AllRules() {
		if fixType := RuleAutoFix(r.ID()); fixType != "" {
			if _, ok := expectedRuleAutoFix[r.ID()]; !ok {
				t.Errorf("rule %q is wired to fixer %q but is not in expectedRuleAutoFix — add it", r.ID(), fixType)
			}
		}
	}
}

// TestRegistry_EveryMetaEntryIsARule catches the other half of a rename: a
// metadata entry under an ID no registered rule has is dead weight that looks
// like working coverage (only SetRuleMetaForTesting may add one).
func TestRegistry_EveryMetaEntryIsARule(t *testing.T) {
	registered := map[string]bool{}
	for _, r := range AllRules() {
		registered[r.ID()] = true
		if _, ok := LookupMeta(r.ID()); !ok {
			t.Errorf("rule %q has no RuleMeta", r.ID())
		}
	}
	for _, r := range LibraryRules() {
		registered[r.ID()] = true
	}
	for id := range metaTable() {
		if !registered[id] {
			t.Errorf("RuleMeta is registered under %q, which is not a registered rule", id)
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
	for _, r := range AllRules() {
		m := MetaOf(r)
		fixes := append([]string(nil), m.DirectFixes...)
		if m.AutoFix != "" {
			fixes = append(fixes, m.AutoFix)
		}
		for _, fixType := range fixes {
			if _, err := PatchForFix(block, fixType, r.ID(), "v", "Url"); err != nil {
				t.Errorf("rule %q advertises fixer %q, which the dispatcher rejects: %v",
					r.ID(), fixType, err)
			}
		}
	}
}
