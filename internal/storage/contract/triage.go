package contract

import (
	"context"
	"encoding/json"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// runFindingStatusScenarios holds the finding triage state.
func runFindingStatusScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	run("ListFindingStatuses_empty_returns_non_nil_slice", func(t *testing.T) {
		got, err := b.ListFindingStatuses(ctx, "no-such-flow-"+runID)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if got == nil {
			t.Errorf("empty finding statuses: expected non-nil empty slice, got nil")
		}
		if len(got) != 0 {
			t.Errorf("empty finding statuses: expected length 0, got %d", len(got))
		}
	})

	run("FindingStatus_upsert_list_delete_roundtrip", func(t *testing.T) {
		owner := "contract-triage-owner-" + runID
		flowID := "contract-flow-triage-" + runID
		if err := b.SaveFlow(ctx, &interfaces.FlowDocument{
			ID: flowID, Name: "Triage Flow", Content: json.RawMessage(`{}`), OwnerID: owner,
		}); err != nil {
			t.Fatalf("SaveFlow: %v", err)
		}

		st := &interfaces.FindingStatus{
			FlowID: flowID, FindingKey: "hardcoded-credential:blk-1", RuleID: "hardcoded-credential",
			Status: "suppressed", Justification: "test secret", AssigneeID: "u1", UpdatedBy: owner,
		}
		if err := b.SetFindingStatus(ctx, st); err != nil {
			t.Fatalf("SetFindingStatus: %v", err)
		}

		got, err := b.ListFindingStatuses(ctx, flowID)
		if err != nil {
			t.Fatalf("ListFindingStatuses: %v", err)
		}
		if len(got) != 1 {
			t.Fatalf("expected 1 status, got %d", len(got))
		}
		if got[0].Status != "suppressed" || got[0].Justification != "test secret" || got[0].AssigneeID != "u1" {
			t.Errorf("status round-trip mismatch: %+v", got[0])
		}
		if got[0].UpdatedAt.IsZero() {
			t.Errorf("expected UpdatedAt to be set by the backend")
		}

		// Upsert: same key updates in place rather than inserting a duplicate.
		st.Status = "resolved"
		if err := b.SetFindingStatus(ctx, st); err != nil {
			t.Fatalf("SetFindingStatus update: %v", err)
		}
		got, _ = b.ListFindingStatuses(ctx, flowID)
		if len(got) != 1 {
			t.Fatalf("upsert should not add a row; got %d", len(got))
		}
		if got[0].Status != "resolved" {
			t.Errorf("upsert: expected status=resolved, got %q", got[0].Status)
		}

		// Delete is idempotent.
		if err := b.DeleteFindingStatus(ctx, flowID, st.FindingKey); err != nil {
			t.Fatalf("DeleteFindingStatus: %v", err)
		}
		if err := b.DeleteFindingStatus(ctx, flowID, st.FindingKey); err != nil {
			t.Fatalf("DeleteFindingStatus (idempotent): %v", err)
		}
		got, _ = b.ListFindingStatuses(ctx, flowID)
		if len(got) != 0 {
			t.Errorf("expected 0 statuses after delete, got %d", len(got))
		}
	})
}

// runFlowBaselineScenarios holds the flow baseline.
func runFlowBaselineScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	run("FlowBaseline_missing_then_set_clear", func(t *testing.T) {
		owner := "contract-baseline-owner-" + runID
		flowID := "contract-flow-baseline-" + runID
		if err := b.SaveFlow(ctx, &interfaces.FlowDocument{
			ID: flowID, Name: "Baseline Flow", Content: json.RawMessage(`{}`), OwnerID: owner,
		}); err != nil {
			t.Fatalf("SaveFlow: %v", err)
		}

		// Missing baseline: (nil, nil), not ErrNotFound.
		bl, err := b.GetFlowBaseline(ctx, flowID)
		if err != nil {
			t.Fatalf("GetFlowBaseline missing: %v", err)
		}
		if bl != nil {
			t.Errorf("expected nil baseline before set, got %+v", bl)
		}

		keys := []string{"r1:b1", "r2:b2"}
		if err := b.SetFlowBaseline(ctx, &interfaces.FlowBaseline{FlowID: flowID, Keys: keys, CreatedBy: owner}); err != nil {
			t.Fatalf("SetFlowBaseline: %v", err)
		}
		bl, err = b.GetFlowBaseline(ctx, flowID)
		if err != nil {
			t.Fatalf("GetFlowBaseline: %v", err)
		}
		if bl == nil || len(bl.Keys) != 2 {
			t.Fatalf("expected baseline with 2 keys, got %+v", bl)
		}
		if bl.CreatedAt.IsZero() {
			t.Errorf("expected CreatedAt to be set by the backend")
		}

		if err := b.SetFlowBaseline(ctx, &interfaces.FlowBaseline{FlowID: flowID, Keys: []string{"r3:b3"}, CreatedBy: owner}); err != nil {
			t.Fatalf("SetFlowBaseline replace: %v", err)
		}
		bl, _ = b.GetFlowBaseline(ctx, flowID)
		if bl == nil || len(bl.Keys) != 1 || bl.Keys[0] != "r3:b3" {
			t.Errorf("expected baseline replaced to single key r3:b3, got %+v", bl)
		}

		if err := b.ClearFlowBaseline(ctx, flowID); err != nil {
			t.Fatalf("ClearFlowBaseline: %v", err)
		}
		bl, _ = b.GetFlowBaseline(ctx, flowID)
		if bl != nil {
			t.Errorf("expected nil baseline after clear, got %+v", bl)
		}
		if err := b.ClearFlowBaseline(ctx, flowID); err != nil {
			t.Errorf("ClearFlowBaseline (idempotent): %v", err)
		}
	})
}
