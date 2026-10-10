package service

import (
	"testing"
)

// Phase 4b review F1: a batch that fixes the same rule on two blocks has two items with one ruleId; the stored
// record matches a decision's per-item outcomes by their id (blockId:ruleId) when it carries one, so the second
// block's outcome does not land on the first. Records written before the id keep matching by ruleId.
func TestJournalTurnRecord_MatchesDecisionItemsByID(t *testing.T) {
	item := func(block, status string) map[string]interface{} {
		return map[string]interface{}{"id": block + ":unhandled-error", "ruleId": "unhandled-error", "blockId": block, "status": status, "summary": "fix " + block}
	}
	_, fixes := journalTurnRecord([]journalEvent{
		{Type: "fix_proposal", Data: map[string]interface{}{"proposalId": "p-batch", "tool": "apply_fixes", "batch": true, "count": 2,
			"items": []map[string]interface{}{item("b1", ""), item("b2", "")}}},
		{Type: "fix_decision", Data: map[string]interface{}{"proposalId": "p-batch", "status": "applied-unresolved",
			"items": []map[string]interface{}{item("b1", "applied"), {"id": "b2:unhandled-error", "ruleId": "unhandled-error", "status": "error", "message": "block changed"}}}},
	})
	if len(fixes) != 1 || len(fixes[0].Items) != 2 {
		t.Fatalf("fixes = %+v, want one record with two items", fixes)
	}
	got := fixes[0].Items
	if got[0].ID != "b1:unhandled-error" || got[1].ID != "b2:unhandled-error" {
		t.Errorf("item ids = %q, %q; want blockId:ruleId kept on the record", got[0].ID, got[1].ID)
	}
	if got[0].Status != "applied" || got[0].Message != "" {
		t.Errorf("item b1 = %+v, want applied", got[0])
	}
	if got[1].Status != "error" || got[1].Message != "block changed" {
		t.Errorf("item b2 = %+v, want its own error, not b1's outcome", got[1])
	}

	// Before the id: one item, matched by ruleId as before.
	_, legacy := journalTurnRecord([]journalEvent{
		{Type: "fix_proposal", Data: map[string]interface{}{"proposalId": "p-old", "items": []map[string]interface{}{{"ruleId": "dead-code", "summary": "x"}}}},
		{Type: "fix_decision", Data: map[string]interface{}{"proposalId": "p-old", "status": "applied", "items": []map[string]interface{}{{"ruleId": "dead-code", "status": "applied"}}}},
	})
	if len(legacy) != 1 || len(legacy[0].Items) != 1 || legacy[0].Items[0].Status != "applied" || legacy[0].Items[0].ID != "" {
		t.Errorf("legacy record = %+v, want its item applied by ruleId with no id", legacy)
	}
}
