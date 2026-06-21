package models

import "testing"

func TestFindingKey(t *testing.T) {
	f := Finding{RuleID: "hardcoded-credential", BlockID: "blk-123"}
	if got, want := f.Key(), "hardcoded-credential:blk-123"; got != want {
		t.Errorf("Key() = %q, want %q", got, want)
	}
}

func TestFindingKey_StableAcrossTitleAndID(t *testing.T) {
	// The same rule firing on the same block is the same finding even if its
	// per-run ID, title, or description differ — Key must ignore those.
	a := Finding{ID: "F-001", RuleID: "r1", BlockID: "b1", Title: "first run wording"}
	b := Finding{ID: "F-009", RuleID: "r1", BlockID: "b1", Title: "reworded later"}
	if a.Key() != b.Key() {
		t.Errorf("expected equal keys, got %q and %q", a.Key(), b.Key())
	}
}

func TestFindingKey_DistinctByRuleAndBlock(t *testing.T) {
	base := Finding{RuleID: "r1", BlockID: "b1"}
	byRule := Finding{RuleID: "r2", BlockID: "b1"}
	byBlock := Finding{RuleID: "r1", BlockID: "b2"}

	if base.Key() == byRule.Key() {
		t.Error("findings from different rules must have different keys")
	}
	if base.Key() == byBlock.Key() {
		t.Error("findings on different blocks must have different keys")
	}
}
