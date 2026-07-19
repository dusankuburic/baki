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

// TestDeduplicateFindings_GroupOrderDeterministic guards M8: report.Groups
// must be sorted (by BlockID, then primary finding title) so output is stable
// across runs despite Go's randomized map iteration. We run the dedup N times
// to amplify any non-determinism and assert the order never varies.
func TestDeduplicateFindings_GroupOrderDeterministic(t *testing.T) {
	findings := []models.Finding{
		{RuleID: "r3", Title: "Zeta rule", BlockID: "block-c"},
		{RuleID: "r1", Title: "Alpha rule", BlockID: "block-a"},
		{RuleID: "r2", Title: "Mid rule", BlockID: "block-b"},
		// Two findings on the same block with different titles exercise the
		// secondary sort key.
		{RuleID: "r4", Title: "Bravo", BlockID: "block-a"},
	}

	wantBlockIDs := []string{"block-a", "block-b", "block-c"}
	// Primary is the FIRST finding encountered for the block, so block-a's
	// primary is whichever title was appended first to that group.
	wantTitles := []string{"Alpha rule", "Mid rule", "Zeta rule"}

	for i := 0; i < 25; i++ {
		_, groups := DeduplicateFindings(findings)
		if len(groups) != len(wantBlockIDs) {
			t.Fatalf("iter %d: got %d groups, want %d", i, len(groups), len(wantBlockIDs))
		}
		for j, g := range groups {
			if g.BlockID != wantBlockIDs[j] {
				t.Fatalf("iter %d group %d: BlockID = %q, want %q", i, j, g.BlockID, wantBlockIDs[j])
			}
			if g.Primary == nil {
				t.Fatalf("iter %d group %d: nil Primary", i, j)
			}
			if g.Primary.Title != wantTitles[j] {
				t.Fatalf("iter %d group %d: Title = %q, want %q", i, j, g.Primary.Title, wantTitles[j])
			}
		}
	}
}
