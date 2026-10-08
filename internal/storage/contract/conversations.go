package contract

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// runConversationScenarios holds the conversation store's round trip and delete.
func runConversationScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(string, func(*testing.T))) {
	run("SaveConversation_then_LoadConversation_round_trips", func(t *testing.T) {
		flowID := "contract-flow-" + runID
		msgs := []interfaces.ChatMessage{
			{ID: "m1", Role: "user", Content: "hi", Timestamp: time.Now().UTC().Format(time.RFC3339)},
			{
				ID: "m2", Role: "assistant", Content: "hello", Timestamp: time.Now().UTC().Format(time.RFC3339),
				TokensIn: 7, TokensOut: 3, Model: "my-deployment",
				// The served model and billed rates of an alias turn (R-3).
				ServedModel: "gpt-4o-2024-11-20", InputCostPerM: 2.5, OutputCostPerM: 10,
			},
		}
		if err := b.SaveConversation(ctx, flowID, "scope", msgs); err != nil {
			t.Fatalf("SaveConversation: %v", err)
		}
		got, err := b.LoadConversation(ctx, flowID, "scope")
		if err != nil {
			t.Fatalf("LoadConversation: %v", err)
		}
		if len(got) != 2 {
			t.Fatalf("round-trip: want 2 messages, got %d", len(got))
		}
		if m := got[1]; m.ServedModel != "gpt-4o-2024-11-20" || m.InputCostPerM != 2.5 || m.OutputCostPerM != 10 || m.Model != "my-deployment" || m.TokensIn != 7 {
			t.Errorf("round-trip lost the served model / billed rates: %+v", m)
		}
	})

	run("DeleteConversation_removes_and_is_idempotent", func(t *testing.T) {
		flowID := "contract-delconv-" + runID
		msgs := []interfaces.ChatMessage{
			{ID: "m1", Role: "user", Content: "hi", Timestamp: time.Now().UTC().Format(time.RFC3339)},
		}
		if err := b.SaveConversation(ctx, flowID, "scope", msgs); err != nil {
			t.Fatalf("SaveConversation: %v", err)
		}
		if err := b.DeleteConversation(ctx, flowID, "scope"); err != nil {
			t.Fatalf("DeleteConversation: %v", err)
		}
		got, err := b.LoadConversation(ctx, flowID, "scope")
		if err != nil {
			t.Fatalf("LoadConversation after delete: %v", err)
		}
		if len(got) != 0 {
			t.Errorf("after delete: want 0 messages, got %d", len(got))
		}
		// Deleting a missing conversation is a no-op, not an error.
		if err := b.DeleteConversation(ctx, flowID, "scope"); err != nil {
			t.Errorf("DeleteConversation (missing) should be a no-op, got %v", err)
		}
	})
}
