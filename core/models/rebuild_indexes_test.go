package models

import (
	"encoding/json"
	"testing"
)

// TestRebuildIndexes_RoundTrip verifies that the transient lookup maps (which
// are not serialized) can be reconstructed after a JSON round-trip, matching
// the indexes a freshly built document would have — including nested blocks.
func TestRebuildIndexes_RoundTrip(t *testing.T) {
	orig := &FlowDocument{
		ID:   "flow-1",
		Name: "Test",
		Subflows: []Subflow{
			{
				ID:   "sf-main",
				Name: "Main",
				Blocks: []Block{
					{ID: "b1", Type: BlockTypeAction},
					{
						ID:   "b2",
						Type: BlockTypeCondition,
						Children: []Block{
							{ID: "b2a", Type: BlockTypeAction},
							{ID: "b2b", Type: BlockTypeAction},
						},
					},
				},
			},
			{
				ID:   "sf-helper",
				Name: "Helper",
				Blocks: []Block{
					{ID: "b3", Type: BlockTypeAction},
				},
			},
		},
	}
	orig.RebuildIndexes()

	// Serialize and deserialize: transient maps are dropped on the wire.
	raw, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	loaded := &FlowDocument{}
	if err := json.Unmarshal(raw, loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if loaded.BlocksByID != nil {
		t.Fatal("expected transient BlocksByID to be nil after unmarshal")
	}

	loaded.RebuildIndexes()

	// Every block (including nested) must be indexed, mapped to its subflow.
	wantBlocks := map[string]string{
		"b1":  "sf-main",
		"b2":  "sf-main",
		"b2a": "sf-main",
		"b2b": "sf-main",
		"b3":  "sf-helper",
	}
	if len(loaded.BlocksByID) != len(wantBlocks) {
		t.Errorf("BlocksByID size = %d, want %d", len(loaded.BlocksByID), len(wantBlocks))
	}
	for id, sfID := range wantBlocks {
		b, ok := loaded.BlocksByID[id]
		if !ok || b == nil {
			t.Errorf("BlocksByID missing %q", id)
			continue
		}
		if b.ID != id {
			t.Errorf("BlocksByID[%q].ID = %q", id, b.ID)
		}
		sf := loaded.BlockSubflow[id]
		if sf == nil || sf.ID != sfID {
			t.Errorf("BlockSubflow[%q] = %v, want subflow %q", id, sf, sfID)
		}
	}
	for _, sfID := range []string{"sf-main", "sf-helper"} {
		if sf := loaded.SubflowsByID[sfID]; sf == nil || sf.ID != sfID {
			t.Errorf("SubflowsByID[%q] missing or wrong", sfID)
		}
	}
}
