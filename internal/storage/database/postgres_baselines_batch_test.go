package database_test

import (
	"context"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// TestPostgres_GetFlowBaselinesBatch pins the batched baseline read the
// governance scanner pages with: one query resolves every listed flow's
// baseline, a flow without one is simply absent, an empty baseline decodes
// with non-nil Keys (as GetFlowBaseline does), and an empty id list is a
// non-nil empty map without a round trip. Gated on DATABASE_URL.
func TestPostgres_GetFlowBaselinesBatch(t *testing.T) {
	b := openTestDB(t)
	ctx := context.Background()

	ids := []string{"blb-keys", "blb-empty", "blb-none"}
	for _, id := range ids {
		if err := b.SaveFlow(ctx, &interfaces.FlowDocument{ID: id, Name: id, Content: []byte(`{"subflows":[]}`)}); err != nil {
			t.Fatalf("SaveFlow %s: %v", id, err)
		}
		id := id
		t.Cleanup(func() { _ = b.DeleteFlow(ctx, id) }) // cascades to flow_baselines
	}
	if err := b.SetFlowBaseline(ctx, &interfaces.FlowBaseline{FlowID: "blb-keys", Keys: []string{"r1:b1", "r2:b2"}, CreatedBy: "u1"}); err != nil {
		t.Fatalf("SetFlowBaseline: %v", err)
	}
	if err := b.SetFlowBaseline(ctx, &interfaces.FlowBaseline{FlowID: "blb-empty", Keys: nil}); err != nil {
		t.Fatalf("SetFlowBaseline (empty): %v", err)
	}

	got, err := b.GetFlowBaselinesBatch(ctx, append(ids, "blb-missing-flow"))
	if err != nil {
		t.Fatalf("GetFlowBaselinesBatch: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("batch returned %d baselines, want 2: %v", len(got), got)
	}
	keys := got["blb-keys"]
	if keys == nil || keys.FlowID != "blb-keys" || keys.CreatedBy != "u1" || len(keys.Keys) != 2 || keys.Keys[0] != "r1:b1" || keys.Keys[1] != "r2:b2" {
		t.Errorf("blb-keys baseline = %+v, want keys [r1:b1 r2:b2] by u1", keys)
	}
	if keys != nil && keys.CreatedAt.IsZero() {
		t.Error("blb-keys baseline has a zero CreatedAt")
	}
	empty := got["blb-empty"]
	if empty == nil || empty.Keys == nil || len(empty.Keys) != 0 {
		t.Errorf("blb-empty baseline = %+v, want non-nil empty Keys", empty)
	}
	if _, ok := got["blb-none"]; ok {
		t.Error("a flow without a baseline must be absent from the batch")
	}

	// Parity with the single read for every listed flow.
	for _, id := range ids {
		single, err := b.GetFlowBaseline(ctx, id)
		if err != nil {
			t.Fatalf("GetFlowBaseline %s: %v", id, err)
		}
		batched := got[id]
		switch {
		case single == nil && batched != nil, single != nil && batched == nil:
			t.Errorf("%s: single = %+v, batched = %+v", id, single, batched)
		case single != nil && len(single.Keys) != len(batched.Keys):
			t.Errorf("%s: single keys %v, batched keys %v", id, single.Keys, batched.Keys)
		}
	}

	none, err := b.GetFlowBaselinesBatch(ctx, nil)
	if err != nil {
		t.Fatalf("GetFlowBaselinesBatch(nil): %v", err)
	}
	if none == nil || len(none) != 0 {
		t.Errorf("empty id list returned %v, want a non-nil empty map", none)
	}
}
