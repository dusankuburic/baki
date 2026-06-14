package analyzer

import (
	"testing"

	"pad-core/models"
)

// TestDeduplicateFindings_KeepsDistinctVariables is the B4 regression test:
// two findings on the same block with the same title but different subject
// variables (e.g. two uninitialized variables) must both survive dedup.
func TestDeduplicateFindings_KeepsDistinctVariables(t *testing.T) {
	findings := []models.Finding{
		{
			RuleID:   "uninitialized-variable",
			Title:    "Variable potentially uninitialized",
			BlockID:  "b1",
			Metadata: map[string]any{"variable": "foo"},
		},
		{
			RuleID:   "uninitialized-variable",
			Title:    "Variable potentially uninitialized",
			BlockID:  "b1",
			Metadata: map[string]any{"variable": "bar"},
		},
	}

	deduped, _ := DeduplicateFindings(findings)
	if len(deduped) != 2 {
		t.Fatalf("distinct variables collapsed: got %d findings, want 2", len(deduped))
	}
}

// TestDeduplicateFindings_CollapsesTrueDuplicates verifies the dedup still
// collapses genuinely identical findings (same block, title, and subject).
func TestDeduplicateFindings_CollapsesTrueDuplicates(t *testing.T) {
	f := models.Finding{
		RuleID:   "uninitialized-variable",
		Title:    "Variable potentially uninitialized",
		BlockID:  "b1",
		Metadata: map[string]any{"variable": "foo"},
	}

	deduped, groups := DeduplicateFindings([]models.Finding{f, f})
	if len(deduped) != 1 {
		t.Fatalf("true duplicates not collapsed: got %d findings, want 1", len(deduped))
	}
	if len(groups) != 1 || groups[0].DuplicateCount != 1 {
		t.Errorf("groups = %+v, want one group with DuplicateCount 1", groups)
	}
}
