package service

import (
	"testing"

	"pad-core/models"
)

// TestBuildMessages_MergesContextIntoSingleUserTurn verifies the core invariant
// behind the consecutive-user-message provider fix: flow context is folded into
// the single trailing user turn, not appended as its own user-role message.
func TestBuildMessages_MergesContextIntoSingleUserTurn(t *testing.T) {
	req := models.ChatRequest{
		Messages: []models.ChatMessage{
			{Role: "user", Content: "earlier question"},
			{Role: "assistant", Content: "earlier answer"},
		},
		UserMessage: "what does this flow do?",
	}
	out := buildMessages(req, "FLOW CONTEXT HERE")

	if len(out) != 3 {
		t.Fatalf("want 3 messages (2 history + 1 merged user turn), got %d", len(out))
	}
	last := out[len(out)-1]
	if last.Role != "user" {
		t.Errorf("last message role = %q, want user", last.Role)
	}
	want := "Context:\nFLOW CONTEXT HERE\n\nwhat does this flow do?"
	if last.Content != want {
		t.Errorf("merged user turn = %q, want %q", last.Content, want)
	}
}

// TestBuildMessages_ExcludeContext verifies ExcludeContext suppresses the merge.
func TestBuildMessages_ExcludeContext(t *testing.T) {
	req := models.ChatRequest{
		UserMessage:    "plain question",
		ExcludeContext: true,
	}
	out := buildMessages(req, "SHOULD NOT APPEAR")
	if len(out) != 1 {
		t.Fatalf("want 1 message, got %d", len(out))
	}
	if out[0].Content != "plain question" {
		t.Errorf("content = %q, want unmodified user message", out[0].Content)
	}
}

// TestBuildMessages_NoContext verifies an empty context is not merged even when
// ExcludeContext is false.
func TestBuildMessages_NoContext(t *testing.T) {
	req := models.ChatRequest{UserMessage: "hi"}
	out := buildMessages(req, "")
	if len(out) != 1 || out[0].Content != "hi" {
		t.Errorf("empty context: got %+v, want single 'hi' user turn", out)
	}
}

// TestBuildMessages_NoConsecutiveUserPairFromContext ensures the context merge
// never introduces a second consecutive user message (the original 400-causing
// bug was context being appended as its own user turn after the history's user
// turn).
func TestBuildMessages_NoConsecutiveUserPairFromContext(t *testing.T) {
	req := models.ChatRequest{
		Messages:    []models.ChatMessage{{Role: "assistant", Content: "a"}},
		UserMessage: "q",
	}
	out := buildMessages(req, "ctx")
	// Exactly one trailing user message is appended; the context did not become a
	// separate message.
	if len(out) != 2 {
		t.Fatalf("want 2 messages, got %d", len(out))
	}
	userCount := 0
	for _, m := range out {
		if m.Role == "user" {
			userCount++
		}
	}
	if userCount != 1 {
		t.Errorf("want exactly 1 user message, got %d", userCount)
	}
}
