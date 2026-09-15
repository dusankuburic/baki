package analyzer

import (
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// TestParsePadIgnore_DoesNotFailOpen is the regression gate for a suppression
// directive that disabled every rule by accident.
//
// The pattern was unanchored with an optional bracket group, so ANY occurrence
// of the word in ANY comment matched with an empty capture and fell through to
// "suppress all". Two shapes mattered in practice: prose that merely mentions
// the directive, and a directive with a mistyped bracket list. Both silenced
// all 45 rules on the following action — Security rules included — and bakicli
// is the CI gate, so this failed OPEN in the one place it must not.
func TestParsePadIgnore_DoesNotFailOpen(t *testing.T) {
	cases := []struct {
		name     string
		text     string
		wantOK   bool
		wantAll  bool
		wantRule string
	}{
		{
			name:   "prose mentioning the directive suppresses nothing",
			text:   "TODO: never use pad-ignore in production flows",
			wantOK: false,
		},
		{
			name:   "documentation reference suppresses nothing",
			text:   "see docs about pad-ignore-next-line usage",
			wantOK: false,
		},
		{
			// Fails CLOSED: the finding the author meant to silence still
			// appears, so the typo is visible rather than silently global.
			name:   "unclosed bracket suppresses nothing",
			text:   "pad-ignore[unhandled-error",
			wantOK: false,
		},
		{
			name:    "bare directive still suppresses everything",
			text:    "pad-ignore",
			wantOK:  true,
			wantAll: true,
		},
		{
			// The blanket form must stand alone. Allowing trailing prose would
			// reintroduce the ambiguity this fix closes — "pad-ignore should
			// never be used in production" opens with the directive too. A
			// justification belongs with the targeted form, which is tested
			// below and may carry free text on either side.
			name:   "blanket directive with trailing prose is not a directive",
			text:   "pad-ignore should never be used in production",
			wantOK: false,
		},
		{
			// Pre-existing, deliberate usage: the reason is written first and
			// the rule list makes the intent explicit. Must keep working.
			name:     "reason before a targeted directive still works",
			text:     "reviewed, false positive pad-ignore[sql-injection-risk]",
			wantOK:   true,
			wantRule: "sql-injection-risk",
		},
		{
			name:     "targeted directive still works",
			text:     "pad-ignore[unhandled-error]",
			wantOK:   true,
			wantRule: "unhandled-error",
		},
		{
			name:     "targeted directive with a trailing reason",
			text:     "pad-ignore[sql-injection-risk] reviewed by security",
			wantOK:   true,
			wantRule: "sql-injection-risk",
		},
		{
			// Documented as case-insensitive; (?i) only ever covered the
			// keyword, so an uppercase rule id matched no rule and quietly
			// suppressed nothing at all.
			name:     "rule ids are case-insensitive as documented",
			text:     "pad-ignore[UNHANDLED-ERROR]",
			wantOK:   true,
			wantRule: "unhandled-error",
		},
		{
			name:    "block-comment framing is stripped",
			text:    "/# pad-ignore #/",
			wantOK:  true,
			wantAll: true,
		},
		{
			name:   "a word merely starting with the directive is not one",
			text:   "pad-ignored files were removed",
			wantOK: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			all, rules, ok := parsePadIgnore(tc.text)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v (rules=%v all=%v)", ok, tc.wantOK, rules, all)
			}
			if all != tc.wantAll {
				t.Errorf("all = %v, want %v — suppressing every rule when the author did not "+
					"ask for it is the fail-open case", all, tc.wantAll)
			}
			if tc.wantRule != "" {
				if len(rules) != 1 || rules[0] != tc.wantRule {
					t.Errorf("rules = %v, want [%s]", rules, tc.wantRule)
				}
			}
		})
	}
}

// suppressionFlow has two actions, each preceded by a comment. Only the first
// carries a directive, and it names only one rule — so it pins BOTH scoping
// dimensions at once.
const suppressionFlow = "#Region \"Main\"\n" +
	"COMMENT pad-ignore[unhandled-error]\n" +
	"File.Delete Files: $'''C:\\\\t\\\\a.txt'''\n" +
	"File.Move Files: $'''C:\\\\b''' Destination: $'''C:\\\\c'''\n" +
	"#EndRegion\n"

func analyzeSuppressed(t *testing.T, src string) (*models.FlowDocument, []models.Finding) {
	t.Helper()
	doc, err := parser.ParseText(src, "Main.txt", int64(len(src)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}
	return doc, RunAnalysis(doc, AllRules(), models.DefaultSettings(), nil).Findings
}

// actionBlockIDs returns the IDs of the non-comment blocks, in source order.
func actionBlockIDs(doc *models.FlowDocument) []string {
	var ids []string
	var walk func(bs []models.Block)
	walk = func(bs []models.Block) {
		for i := range bs {
			if bs[i].Type != models.BlockTypeComment {
				ids = append(ids, bs[i].ID)
			}
			walk(bs[i].Children)
		}
	}
	for i := range doc.Subflows {
		walk(doc.Subflows[i].Blocks)
	}
	return ids
}

// TestInlineSuppression_IsScopedToBothRuleAndBlock covers the ENFORCEMENT side,
// which had no test of its own.
//
// The existing end-to-end test asserted only that the targeted finding vanished
// and that the total decreased. Both mutations of the match condition satisfy
// that: dropping the rule check (any directive silences every rule on the
// block) and dropping the block check (any directive silences that rule
// flow-wide) each pass it. Construction of the suppression map was tested;
// application of it was not.
func TestInlineSuppression_IsScopedToBothRuleAndBlock(t *testing.T) {
	doc, findings := analyzeSuppressed(t, suppressionFlow)
	ids := actionBlockIDs(doc)
	if len(ids) < 2 {
		t.Fatalf("fixture rot: expected 2 action blocks, got %d", len(ids))
	}
	target, other := ids[0], ids[1]

	var suppressedOnTarget, sameRuleElsewhere, otherRuleOnTarget int
	for _, f := range findings {
		switch {
		case f.RuleID == "unhandled-error" && f.BlockID == target:
			suppressedOnTarget++
		case f.RuleID == "unhandled-error" && f.BlockID == other:
			sameRuleElsewhere++
		case f.BlockID == target:
			otherRuleOnTarget++
		}
	}

	if suppressedOnTarget != 0 {
		t.Errorf("the directive did not suppress its own rule on its own block (%d finding(s))",
			suppressedOnTarget)
	}
	// Rule scoping: OTHER rules on the SAME block must survive.
	if otherRuleOnTarget == 0 {
		t.Error("a directive naming one rule silenced every rule on its block — " +
			"the rule check is not enforced")
	}
	// Block scoping: the SAME rule on OTHER blocks must survive.
	if sameRuleElsewhere == 0 {
		t.Error("a directive on one block silenced its rule flow-wide — " +
			"the block check is not enforced")
	}
}

// TestInlineSuppression_ProseDoesNotSilenceTheFlow is the end-to-end companion:
// the unit test above proves the parse, this proves it reaches the report.
//
// It compares findings on the ACTION block only. Comparing raw totals would be
// wrong: the comment line is itself a block and draws its own findings, so the
// two reports legitimately differ in length.
func TestInlineSuppression_ProseDoesNotSilenceTheFlow(t *testing.T) {
	const action = "File.Delete Files: $'''C:\\\\t\\\\a.txt'''\n"
	prose := "#Region \"Main\"\nCOMMENT never use pad-ignore in production flows\n" + action + "#EndRegion\n"
	bare := "#Region \"Main\"\n" + action + "#EndRegion\n"

	countOn := func(src string) int {
		doc, findings := analyzeSuppressed(t, src)
		ids := actionBlockIDs(doc)
		if len(ids) != 1 {
			t.Fatalf("fixture rot: expected 1 action block, got %d", len(ids))
		}
		n := 0
		for _, f := range findings {
			if f.BlockID == ids[0] {
				n++
			}
		}
		return n
	}

	withProse, without := countOn(prose), countOn(bare)
	if without == 0 {
		t.Fatal("fixture rot: the bare flow produces no findings on the action, so this asserts nothing")
	}
	if withProse != without {
		t.Errorf("a comment merely MENTIONING pad-ignore changed the action's findings: %d vs %d "+
			"without the comment — prose is disabling rules", withProse, without)
	}
}
