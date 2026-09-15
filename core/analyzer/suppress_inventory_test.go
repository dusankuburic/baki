package analyzer

import (
	"testing"

	"pad-core/models"
)

func inventoryEntries(t *testing.T, flow *models.FlowDocument) []SuppressionEntry {
	t.Helper()
	return SuppressionInventory(flow, AllRules(), models.DefaultSettings())
}

// TestSuppressionInventory_HeaderDirectiveAuditsHeaderScope: a top-of-file
// `pad-ignore[parse-error]` is listed, and it is live while parse errors exist
// (they are anchored on the directive comment / on no block — targets the
// next-block audit alone cannot see) and stale once they are gone.
func TestSuppressionInventory_HeaderDirectiveAuditsHeaderScope(t *testing.T) {
	broken := "#Region \"Main\"\n# pad-ignore[parse-error]\nWebAutomation.Click Element: 'btn'\nELSE\n#EndRegion\n"
	clean := "#Region \"Main\"\n# pad-ignore[parse-error]\nWebAutomation.Click Element: 'btn'\n#EndRegion\n"

	got := inventoryEntries(t, parseFlow(t, broken))
	if len(got) != 1 || got[0].Rule != "parse-error" {
		t.Fatalf("expected exactly the parse-error directive, got %+v", got)
	}
	if got[0].Stale {
		t.Errorf("directive silences live parse errors but is reported stale: %+v", got[0])
	}

	got = inventoryEntries(t, parseFlow(t, clean))
	if len(got) != 1 || got[0].Rule != "parse-error" {
		t.Fatalf("expected exactly the parse-error directive, got %+v", got)
	}
	if !got[0].Stale {
		t.Errorf("directive masks nothing but is reported live: %+v", got[0])
	}
}

// TestSuppressionInventory_FallbackParseErrorIsLive mirrors the engine's
// document-level fallback (first subflow without blocks): the directive at the
// top of the first subflow WITH blocks silences it and must be reported live.
func TestSuppressionInventory_FallbackParseErrorIsLive(t *testing.T) {
	cmt := makeBlock("c1", "pad-ignore[parse-error]", models.BlockTypeComment, "COMMENT", 0)
	cmt.LineNumber = 2
	act := makeBlock("b1", "Click", models.BlockTypeAction, "WebAutomation.Click", 0)
	act.SubflowID = "sf2"
	flow := &models.FlowDocument{
		ID:          "t",
		ParseErrors: []models.ParseError{{Line: 1, Message: "malformed line", Severity: "error"}},
		Subflows: []models.Subflow{
			{ID: "sf1", Name: "Main"},
			{ID: "sf2", Name: "Helper", Blocks: []models.Block{*cmt, *act}},
		},
	}
	got := inventoryEntries(t, flow)
	if len(got) != 1 || got[0].Rule != "parse-error" || got[0].Stale {
		t.Fatalf("expected one live parse-error directive, got %+v", got)
	}
	if got[0].Subflow != "Helper" || got[0].BlockLabel != "Click" {
		t.Errorf("entry should still describe the next block: %+v", got[0])
	}
}

// TestSuppressionInventory_NextBlockAuditUnchanged keeps the original
// semantics: a directive on a block where the rule fires is live, one where it
// does not is stale, and a directive with nothing after it is stale.
func TestSuppressionInventory_NextBlockAuditUnchanged(t *testing.T) {
	action := func(id string) models.Block {
		b := makeBlock(id, "Click button", models.BlockTypeAction, "WebAutomation.Click", 0)
		b.SubflowID = "sf1"
		return *b
	}
	flow := &models.FlowDocument{ID: "t", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{
		action("a0"),
		*makeBlock("c1", "pad-ignore[unhandled-error]", models.BlockTypeComment, "COMMENT", 0),
		action("a1"),
		*makeBlock("c2", "pad-ignore[parse-error]", models.BlockTypeComment, "COMMENT", 0),
		action("a2"),
		*makeBlock("c3", "pad-ignore[unhandled-error]", models.BlockTypeComment, "COMMENT", 0),
	}}}}
	got := inventoryEntries(t, flow)
	if len(got) != 3 {
		t.Fatalf("expected 3 entries, got %+v", got)
	}
	byLabel := map[string]SuppressionEntry{}
	for _, e := range got {
		byLabel[e.Rule+"@"+e.BlockLabel] = e
	}
	if e := byLabel["unhandled-error@Click button"]; e.Stale && e.Reason != "" {
		// a1 is a fallible action with no handler: unhandled-error fires there.
		t.Errorf("live next-block directive reported stale: %+v", e)
	}
	if e, ok := byLabel["parse-error@Click button"]; !ok || !e.Stale {
		t.Errorf("mid-subflow parse-error directive with no parse errors should be stale: %+v", e)
	}
	if e, ok := byLabel["unhandled-error@<no following block>"]; !ok || !e.Stale || e.Reason != "directive has no following block" {
		t.Errorf("trailing directive: %+v", e)
	}
}
