package analyzer

import (
	"testing"

	"pad-core/models"
)

// relFinding builds a finding on blockID whose Metadata["variables"] lists the
// given variable names (nil metadata when none are given).
func relFinding(ruleID, blockID string, vars ...string) models.Finding {
	f := models.Finding{RuleID: ruleID, BlockID: blockID}
	if len(vars) > 0 {
		f.Metadata = map[string]interface{}{"variables": vars}
	}
	return f
}

// A finding on another block that shares a variable with the anchor block is
// "related"; findings sharing nothing, and findings on the anchor block itself,
// are not returned.
func TestFindRelatedFindings_SharedVariable(t *testing.T) {
	findings := []models.Finding{
		relFinding("r1", "A", "x"),      // anchor reference
		relFinding("r2", "B", "x", "y"), // shares x → related
		relFinding("r3", "C", "z"),      // shares nothing → not related
		relFinding("r4", "A", "x"),      // same block as anchor → excluded
	}

	related := FindRelatedFindings(findings, "A")
	if len(related) != 1 {
		t.Fatalf("expected 1 related finding, got %d", len(related))
	}
	if related[0].BlockID != "B" {
		t.Errorf("related block = %q, want B", related[0].BlockID)
	}
}

func TestFindRelatedFindings_NoSharedVariable_Empty(t *testing.T) {
	findings := []models.Finding{
		relFinding("r1", "A", "x"),
		relFinding("r2", "B", "y"),
	}
	if related := FindRelatedFindings(findings, "A"); len(related) != 0 {
		t.Errorf("expected 0 related findings, got %d", len(related))
	}
}

// When the anchor block has no variable metadata there is nothing to relate to;
// the result must be a non-nil empty slice (JSON-safe).
func TestFindRelatedFindings_AnchorWithoutVariables_EmptyNonNil(t *testing.T) {
	findings := []models.Finding{
		relFinding("r1", "A"),
		relFinding("r2", "B", "x"),
	}
	related := FindRelatedFindings(findings, "A")
	if len(related) != 0 {
		t.Errorf("expected 0 related findings, got %d", len(related))
	}
	if related == nil {
		t.Error("expected a non-nil empty slice")
	}
}
