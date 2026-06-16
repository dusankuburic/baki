package service

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/config"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// In cloud mode, conversations must persist through the storage backend (durable
// + cross-replica) rather than the local filesystem, and the model↔storage
// bridge must preserve every domain field.
func TestChatService_CloudConversationRoundTrip(t *testing.T) {
	backend := testutil.NewFakeBackend()
	svc := &ChatService{backend: backend, mode: config.ModeCloud, configDir: t.TempDir()}
	doc := &models.FlowDocument{ID: "flow-1"}
	ctx := context.Background()

	ts := time.Date(2026, 6, 15, 10, 30, 0, 0, time.UTC)
	msgs := []models.ChatMessage{
		{ID: "1", Role: "user", Content: "hello", Timestamp: ts, ContextBlockID: "b1", ContextSubflowID: "s1"},
		{ID: "2", Role: "assistant", Content: "hi", Provider: "claude", Model: "claude-sonnet-4-6", FinishReason: "stop", TokensIn: 12, TokensOut: 7},
	}

	if err := svc.SaveConversation(ctx, doc, "claude", msgs); err != nil {
		t.Fatalf("SaveConversation: %v", err)
	}
	// It went to the backend, not the filesystem.
	if len(backend.Conversations) == 0 {
		t.Fatal("conversation was not persisted to the backend in cloud mode")
	}

	got, err := svc.GetConversation(ctx, doc, "claude")
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d messages, want 2", len(got))
	}
	// Every domain field survives the round trip through interfaces.ChatMessage.
	if got[0].ContextBlockID != "b1" || got[0].ContextSubflowID != "s1" || !got[0].Timestamp.Equal(ts) {
		t.Errorf("message[0] fields lost: %+v", got[0])
	}
	if got[1].Provider != "claude" || got[1].Model != "claude-sonnet-4-6" ||
		got[1].FinishReason != "stop" || got[1].TokensIn != 12 || got[1].TokensOut != 7 {
		t.Errorf("message[1] fields lost: %+v", got[1])
	}

	// Clear deletes it; a later read returns empty, not an error.
	if err := svc.ClearConversation(ctx, doc, "claude"); err != nil {
		t.Fatalf("ClearConversation: %v", err)
	}
	if got, err := svc.GetConversation(ctx, doc, "claude"); err != nil || len(got) != 0 {
		t.Errorf("after clear: got %d msgs, err %v", len(got), err)
	}
}

// Local mode keeps the filesystem path even when a backend is present, so the
// desktop experience is unchanged.
func TestChatService_LocalModeUsesFilesystem(t *testing.T) {
	backend := testutil.NewFakeBackend()
	svc := &ChatService{backend: backend, mode: config.ModeLocal, configDir: t.TempDir()}
	doc := &models.FlowDocument{ID: "flow-1"}
	ctx := context.Background()

	if err := svc.SaveConversation(ctx, doc, "claude", []models.ChatMessage{{ID: "1", Role: "user", Content: "hi"}}); err != nil {
		t.Fatalf("SaveConversation: %v", err)
	}
	if len(backend.Conversations) != 0 {
		t.Error("local mode wrote to the backend; expected filesystem only")
	}
	got, err := svc.GetConversation(ctx, doc, "claude")
	if err != nil || len(got) != 1 {
		t.Fatalf("GetConversation: got %d err %v", len(got), err)
	}
}
