package analyzer

import (
	"strings"
	"testing"

	"pad-core/models"
)

// TestFindingContentKey_StableAcrossReparse verifies the headline property of
// content-stable fingerprints (0.1): the same finding — same rule, same block
// content, same line — produces the SAME key even when the parser mints fresh
// BlockID/SubflowID UUIDs on a re-parse. The legacy Key() (RuleID:BlockID)
// would differ; findingContentKey must not.
func TestFindingContentKey_StableAcrossReparse(t *testing.T) {
	// Two independently-parsed docs of the same source: same block content +
	// line, different parser-minted IDs.
	mkFlow := func(blockID, subflowID string) *models.FlowDocument {
		sf := &models.Subflow{ID: subflowID, Name: "Main"}
		block := &models.Block{ID: blockID, Name: "Connect DB", Type: models.BlockTypeAction,
			RawType: "Database.Connect", LineNumber: 12, SubflowID: subflowID}
		sf.Blocks = []models.Block{*block}
		doc := &models.FlowDocument{ID: "f1", Subflows: []models.Subflow{*sf}}
		doc.RebuildIndexes()
		return doc
	}
	docA := mkFlow("aaaaaaaa-0000-0000-0000-000000000001", "ssssssss-0000-0000-0000-000000000001")
	docB := mkFlow("bbbbbbbb-0000-0000-0000-000000000002", "tttttttt-0000-0000-0000-000000000002")

	fA := models.Finding{RuleID: "unhandled-error", BlockID: "aaaaaaaa-0000-0000-0000-000000000001"}
	fB := models.Finding{RuleID: "unhandled-error", BlockID: "bbbbbbbb-0000-0000-0000-000000000002"}

	keyA := findingContentKey(fA, docA)
	keyB := findingContentKey(fB, docB)
	if keyA != keyB {
		t.Errorf("content key changed across a re-parse of the same source: %q vs %q (legacy Key would be %q vs %q)", keyA, keyB, fA.Key(), fB.Key())
	}
	if !strings.HasPrefix(keyA, "unhandled-error:") {
		t.Errorf("content key should be rule-prefixed, got %q", keyA)
	}

	// A different rule on the same block → different key.
	fOther := models.Finding{RuleID: "missing-timeout", BlockID: fA.BlockID}
	if findingContentKey(fOther, docA) == keyA {
		t.Error("different rule on the same block must produce a different content key")
	}

	// A different subject (variable) on the same rule+block → different key.
	withVar := fA
	withVar.Metadata = map[string]interface{}{"variable": "conn"}
	noVar := fA
	if findingContentKey(withVar, docA) == findingContentKey(noVar, docA) {
		t.Error("a finding with a subject must differ from the same finding without one")
	}
}

// TestFindingContentKey_NilFlowFallback confirms the helper degrades to the
// legacy Key() (still unique within a run) when block context is unavailable.
func TestFindingContentKey_NilFlowFallback(t *testing.T) {
	f := models.Finding{RuleID: "r1", BlockID: "b1"}
	if got := findingContentKey(f, nil); got != f.Key() {
		t.Errorf("nil flow should fall back to Key(), got %q want %q", got, f.Key())
	}
	doc := &models.FlowDocument{ID: "f1"} // no blocks indexed
	doc.RebuildIndexes()
	if got := findingContentKey(f, doc); got != f.Key() {
		t.Errorf("unknown block should fall back to Key(), got %q want %q", got, f.Key())
	}
}
