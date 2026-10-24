package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// What the chat store holds is the scrubbed copy of every message, so /api/chat/get hands a viewer holding the
// thread's key the masked text, never the connection string or token the author pasted.

const (
	chatScrubFlow   = "f-cv"
	chatScrubKey    = "11111111-2222-4333-8444-555555555555"
	chatBearerValue = "abcdefghijklmnopqrstuvwxyz0123456789"
	chatBearerText  = "the header I send is Authorization: Bearer " + chatBearerValue + " and it is refused"
	chatSecretText  = "it fails to connect with Password=hunter2secret from the desktop"
	chatSecretValue = "hunter2secret"
)

// chatGetBody is what /api/chat/get answers, as these tests read it.
type chatGetBody struct {
	Messages       []models.ChatMessage `json:"messages"`
	ActiveStreamID string               `json:"activeStreamId"`
}

func chatGet(t *testing.T, rt *Router, auth string) (chatGetBody, string) {
	t.Helper()
	rr := chatPost(t, rt, auth, "/api/chat/get", map[string]any{})
	checkStatus(t, rr, http.StatusOK)
	var body chatGetBody
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode %s: %v", rr.Body.String(), err)
	}
	return body, rr.Body.String()
}

// storedChatContent is the content of message id as the backend holds it.
func storedChatContent(t *testing.T, rt *Router, id string) string {
	t.Helper()
	stored, err := rt.security.Backend.LoadConversation(context.Background(), chatScrubFlow, chatScrubKey)
	if err != nil {
		t.Fatalf("LoadConversation: %v", err)
	}
	for _, m := range stored {
		if m.ID == id {
			return m.Content
		}
	}
	t.Fatalf("the store does not hold message %s: %+v", id, stored)
	return ""
}

func assertChatMasked(t *testing.T, what, content, secret string) {
	t.Helper()
	if strings.Contains(content, secret) {
		t.Errorf("%s holds the secret in plain text: %q", what, content)
	}
	if !strings.Contains(content, "[REDACTED]") {
		t.Errorf("%s carries no mask: %q", what, content)
	}
}

// A save carrying a Bearer token stores it masked, with baseVersion (the compare-and-swap) and without (the blind
// save), and the next GET answers the masked text.
func TestChatSave_SecretsAreStoredMasked(t *testing.T) {
	cases := []struct {
		name string
		body map[string]any
	}{
		{"with baseVersion", map[string]any{"baseVersion": 0, "messages": []any{chatMsg("m1", chatBearerText)}}},
		{"without baseVersion", map[string]any{"messages": []any{chatMsg("m1", chatBearerText)}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rt, auth := chatVersionRouter(t)
			rr := chatPost(t, rt, auth, "/api/chat/save", tc.body)
			checkStatus(t, rr, http.StatusOK)

			assertChatMasked(t, "the stored message", storedChatContent(t, rt, "m1"), chatBearerValue)
			got, raw := chatGet(t, rt, auth)
			if strings.Contains(raw, chatBearerValue) {
				t.Errorf("GET answers the token in plain text: %s", raw)
			}
			if len(got.Messages) != 1 {
				t.Fatalf("GET answered %d messages, want 1: %s", len(got.Messages), raw)
			}
			assertChatMasked(t, "the message GET answers", got.Messages[0].Content, chatBearerValue)
		})
	}
}

// A streamed turn whose question carries Password=... is stored masked, and GET answers the masked question.
func TestChatGet_StreamedQuestionIsMasked(t *testing.T) {
	rt, auth := chatVersionRouter(t)
	provider := "cv-scrub-" + uuid.NewString()
	ai.RegisterProvider(provider, func(string) ai.Provider {
		return &testutil.FakeProvider{Fallback: &testutil.FakeTurn{Text: "the answer", TokensIn: 1, TokensOut: 1}}
	})
	svc := moduleOf[*ChatHandler](t, rt).chatSvc
	t.Cleanup(svc.CancelAll)
	req := models.ChatRequest{
		Provider: provider, Model: "fake-model", ConversationID: chatScrubKey, UserMessage: chatSecretText,
		UserMessageID: uuid.NewString(), AssistantMessageID: uuid.NewString(), ClientStreamID: uuid.NewString(),
	}
	if _, err := svc.StreamChatMessage(context.Background(), "u1", &models.FlowDocument{ID: chatScrubFlow, Name: "t"}, nil, req); err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}

	// The turn is over when the answer is stored and no stream is reported on the thread.
	deadline := time.Now().Add(5 * time.Second)
	var got chatGetBody
	var raw string
	for {
		got, raw = chatGet(t, rt, auth)
		if len(got.Messages) == 2 && got.ActiveStreamID == "" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("the turn did not settle: %s", raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if strings.Contains(raw, chatSecretValue) {
		t.Errorf("GET answers the password in plain text: %s", raw)
	}
	assertChatMasked(t, "the stored question", storedChatContent(t, rt, req.UserMessageID), chatSecretValue)
	for _, m := range got.Messages {
		if m.ID == req.UserMessageID {
			assertChatMasked(t, "the question GET answers", m.Content, chatSecretValue)
			return
		}
	}
	t.Fatalf("GET does not answer the question %s: %s", req.UserMessageID, raw)
}
