package analyzer

import (
	"testing"

	"pad-core/models"
)

// relFinding builds a finding on blockID whose Metadata carries the given
// subject under the key a production rule would write ("variable"). Production
// rules set exactly one subject per finding (variable/property/resource).
func relFinding(ruleID, blockID, subject string) models.Finding {
	f := models.Finding{RuleID: ruleID, BlockID: blockID}
	if subject != "" {
		f.Metadata = map[string]interface{}{"variable": subject}
	}
	return f
}

// A finding on another block that shares a subject with the anchor block is
// "related"; findings sharing nothing, and findings on the anchor block itself,
// are not returned.
func TestFindRelatedFindings_SharedVariable(t *testing.T) {
	findings := []models.Finding{
		relFinding("r1", "A", "x"), // anchor reference
		relFinding("r2", "B", "x"), // shares x → related
		relFinding("r3", "C", "z"), // shares nothing → not related
		relFinding("r4", "A", "x"), // same block as anchor → excluded
	}

	related := FindRelatedFindings(findings, "A")
	if len(related) != 1 {
		t.Fatalf("expected 1 related finding, got %d", len(related))
	}
	if related[0].BlockID != "B" {
		t.Errorf("related block = %q, want B", related[0].BlockID)
	}
}

// Related-findings must also match across the other subject key types rules
// write (property/resource), not just "variable".
func TestFindRelatedFindings_SharedResource(t *testing.T) {
	findings := []models.Finding{
		{RuleID: "r1", BlockID: "A", Metadata: map[string]interface{}{"resource": "conn"}},
		{RuleID: "r2", BlockID: "B", Metadata: map[string]interface{}{"resource": "conn"}},
		{RuleID: "r3", BlockID: "C", Metadata: map[string]interface{}{"resource": "other"}},
	}
	related := FindRelatedFindings(findings, "A")
	if len(related) != 1 || related[0].BlockID != "B" {
		t.Fatalf("expected 1 related (B) via shared resource, got %+v", related)
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

// When the anchor block has no subject metadata there is nothing to relate to;
// the result must be a non-nil empty slice (JSON-safe).
func TestFindRelatedFindings_AnchorWithoutVariables_EmptyNonNil(t *testing.T) {
	findings := []models.Finding{
		relFinding("r1", "A", ""),
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
