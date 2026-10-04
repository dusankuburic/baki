package ai

import (
	"encoding/json"
	"testing"
)

// TestReplayStoredItems_PassthroughAndFallback pins the fast replay path:
// items that replayableItems already normalised replay byte-for-byte (no
// field decode, no re-encode), and anything that cannot be PROVEN unchanged
// declines to the normaliser.
func TestReplayStoredItems_PassthroughAndFallback(t *testing.T) {
	normalized := []json.RawMessage{
		json.RawMessage(`{"type":"message","id":"m1","status":"completed","content":"hi"}`),
		json.RawMessage(`{"encrypted_content":"ENC","id":"rs1","summary":[],"type":"reasoning"}`),
	}
	got, ok := replayStoredItems(normalized)
	if !ok || len(got) != 2 {
		t.Fatalf("normalized items must replay: ok=%v items=%v", ok, got)
	}
	for i := range normalized {
		if string(got[i]) != string(normalized[i]) {
			t.Fatalf("item %d re-encoded: got %s want %s", i, got[i], normalized[i])
		}
	}

	cases := []struct {
		name string
		item string
	}{
		{"foundry annotation", `{"agent_reference":"a","id":"m","status":"completed","type":"message"}`},
		{"response id annotation", `{"id":"m","response_id":"r","status":"completed","type":"message"}`},
		{"non-replayable type", `{"type":"workflow_action"}`},
		{"reasoning without summary", `{"encrypted_content":"E","id":"rs","type":"reasoning"}`},
		{"message without status", `{"id":"m","type":"message"}`},
	}
	for _, c := range cases {
		if _, ok := replayStoredItems([]json.RawMessage{json.RawMessage(c.item)}); ok {
			t.Errorf("%s: item must decline to the normaliser: %s", c.name, c.item)
		}
	}
}
