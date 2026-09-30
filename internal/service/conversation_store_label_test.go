package service

import (
	"context"
	"encoding/json"
	"testing"

	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// A message's modelLabel — the Azure registry entry's display name when it
// was sent — survives both persistence paths and the storage bridge; without
// the mirror field or the bridge copy it would be dropped on every cloud
// save (Part 9).
func TestConversationStore_ModelLabelRoundTrip(t *testing.T) {
	msgs := []models.ChatMessage{
		{ID: "1", Role: "user", Content: "hi"},
		{ID: "2", Role: "assistant", Content: "hello", Provider: "azure-foundry", Model: "m/abcdefghij", ModelLabel: "Docs writer", ServedModel: "gpt-4.1-2025-04-14"},
	}

	// The storage bridge, both ways.
	back := toModelMessages(toStorageMessages(msgs))
	if back[1].ModelLabel != "Docs writer" || back[1].Model != "m/abcdefghij" || back[0].ModelLabel != "" {
		t.Fatalf("bridge = %+v", back)
	}
	// The storage mirror's JSON carries it (the database stores the array).
	raw, err := json.Marshal(toStorageMessages(msgs))
	if err != nil {
		t.Fatal(err)
	}
	var generic []map[string]any
	_ = json.Unmarshal(raw, &generic)
	if generic[1]["modelLabel"] != "Docs writer" {
		t.Errorf("storage JSON = %s", raw)
	}
	if _, present := generic[0]["modelLabel"]; present {
		t.Errorf("an empty label must be omitted: %s", raw)
	}

	// The cloud path end to end (backend conversation store).
	backend := testutil.NewFakeBackend()
	if err := backend.SaveConversation(context.Background(), "flow-1", "u1", toStorageMessages(msgs)); err != nil {
		t.Fatal(err)
	}
	stored, _ := backend.LoadConversation(context.Background(), "flow-1", "u1")
	if got := toModelMessages(stored); got[1].ModelLabel != "Docs writer" {
		t.Errorf("backend round trip = %+v", got)
	}

	// The filesystem path (desktop): the domain type serialises verbatim.
	svc := &ChatService{configDir: t.TempDir()}
	doc := &models.FlowDocument{ID: "flow-1"}
	if err := svc.SaveConversation(context.Background(), doc, "flow", msgs); err != nil {
		t.Fatal(err)
	}
	got, err := svc.GetConversation(context.Background(), doc, "flow")
	if err != nil || len(got) != 2 || got[1].ModelLabel != "Docs writer" {
		t.Errorf("filesystem round trip = %+v, %v", got, err)
	}
}
