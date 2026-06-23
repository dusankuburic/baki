package analyzer

import (
	"testing"

	"pad-core/models"
)

func TestParsePadIgnore(t *testing.T) {
	tests := []struct {
		name    string
		text    string
		wantOK  bool
		wantAll bool
		wantN   int
	}{
		{name: "no directive", text: "just a normal comment", wantOK: false},
		{name: "bare suppress all", text: "pad-ignore", wantOK: true, wantAll: true},
		{name: "next-line alias", text: "pad-ignore-next-line", wantOK: true, wantAll: true},
		{name: "empty brackets = all", text: "pad-ignore[]", wantOK: true, wantAll: true},
		{name: "single rule", text: "pad-ignore[hardcoded-credential]", wantOK: true, wantN: 1},
		{name: "multiple rules", text: "pad-ignore[deep-nesting, unused-variable]", wantOK: true, wantN: 2},
		{name: "embedded in prose", text: "reviewed, false positive pad-ignore[sql-injection-risk]", wantOK: true, wantN: 1},
		{name: "case insensitive", text: "PAD-IGNORE[unhandled-error]", wantOK: true, wantN: 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			all, rules, ok := parsePadIgnore(tc.text)
			if ok != tc.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tc.wantOK)
			}
			if !ok {
				return
			}
			if all != tc.wantAll {
				t.Errorf("all = %v, want %v", all, tc.wantAll)
			}
			if !tc.wantAll && len(rules) != tc.wantN {
				t.Errorf("rules = %v (len %d), want len %d", rules, len(rules), tc.wantN)
			}
		})
	}
}

func TestCollectInlineSuppressions_NextSiblingOnly(t *testing.T) {
	comment := makeBlock("c1", "pad-ignore[unhandled-error]", models.BlockTypeComment, "COMMENT", 0)
	target := makeBlock("b1", "Click", models.BlockTypeAction, "WebAutomation.Click", 0)
	after := makeBlock("b2", "Click again", models.BlockTypeAction, "WebAutomation.Click", 0)
	flow := &models.FlowDocument{ID: "t", Subflows: []models.Subflow{
		{ID: "sf1", Name: "Main", Blocks: []models.Block{*comment, *target, *after}},
	}}

	supp := collectInlineSuppressions(flow)
	if m := supp["b1"]; m == nil || !m["unhandled-error"] {
		t.Fatalf("expected b1 to suppress unhandled-error, got %v", supp["b1"])
	}
	if _, ok := supp["b2"]; ok {
		t.Errorf("directive must not leak to the block after the target: %v", supp["b2"])
	}
}

// TestInlineSuppression_EndToEnd drives the full analyzer: a fallible action
// triggers unhandled-error, and a preceding `# pad-ignore` comment must remove it
// from the report and bump stats.Suppressed.
func TestInlineSuppression_EndToEnd(t *testing.T) {
	newFlow := func(blocks ...models.Block) *models.FlowDocument {
		return &models.FlowDocument{ID: "t", Subflows: []models.Subflow{
			{ID: "sf1", Name: "Main", Blocks: blocks},
		}}
	}
	action := func(id string) models.Block {
		b := makeBlock(id, "Click button", models.BlockTypeAction, "WebAutomation.Click", 0)
		b.SubflowID = "sf1"
		return *b
	}

	// Baseline: one unhandled-error finding, nothing suppressed.
	base := RunAnalysis(newFlow(action("b1")), AllRules(), nil, nil)
	if base.Stats.Errors+base.Stats.Warnings == 0 {
		t.Fatalf("expected the fallible action to produce a finding in baseline")
	}
	baseCount := len(base.Findings)

	// Targeted suppression removes exactly that rule's finding.
	cmt := makeBlock("c1", "pad-ignore[unhandled-error]", models.BlockTypeComment, "COMMENT", 0)
	rep := RunAnalysis(newFlow(*cmt, action("b1")), AllRules(), nil, nil)
	for _, f := range rep.Findings {
		if f.RuleID == "unhandled-error" && f.BlockID == "b1" {
			t.Fatalf("unhandled-error on b1 should have been suppressed")
		}
	}
	if rep.Stats.Suppressed < 1 {
		t.Errorf("Stats.Suppressed = %d, want >= 1", rep.Stats.Suppressed)
	}
	if len(rep.Findings) >= baseCount {
		t.Errorf("expected fewer findings after suppression: got %d, baseline %d", len(rep.Findings), baseCount)
	}

	// Bare `pad-ignore` suppresses everything on the next block.
	cmtAll := makeBlock("c2", "pad-ignore", models.BlockTypeComment, "COMMENT", 0)
	repAll := RunAnalysis(newFlow(*cmtAll, action("b1")), AllRules(), nil, nil)
	for _, f := range repAll.Findings {
		if f.BlockID == "b1" {
			t.Fatalf("bare pad-ignore should suppress all findings on b1, got %s", f.RuleID)
		}
	}
}
