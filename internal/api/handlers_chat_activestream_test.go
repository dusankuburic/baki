package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/storage/filesystem"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// blockingStreamProvider streams a preamble and then blocks until released,
// so the stream stays live for the activeStreamId assertions.
type blockingStreamProvider struct {
	*testutil.FakeProvider
	release   chan struct{}
	closeOnce sync.Once
}

func (p *blockingStreamProvider) Stream(ctx context.Context, _ ai.Request, onChunk func(ai.Chunk)) error {
	onChunk(ai.Chunk{Text: "partial…"})
	select {
	case <-p.release:
	case <-ctx.Done():
		return ctx.Err()
	}
	onChunk(ai.Chunk{Text: " done"})
	onChunk(ai.Chunk{Done: true, FinishReason: "stop", TokensIn: 1, TokensOut: 1})
	return nil
}

// getConversationBody is the /api/chat/get response shape.
type getConversationBody struct {
	Messages       []json.RawMessage `json:"messages"`
	ActiveStreamID string            `json:"activeStreamId"`
}

// pollConversation re-reads the conversation until ok accepts the body, or
// fails the test after 5 s with the last body read.
func pollConversation(t *testing.T, rt *Router, auth, convID string, ok func(getConversationBody) bool) getConversationBody {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/get",
			auth, map[string]string{"flowId": "f-f21", "provider": convID})
		checkStatus(t, rr, http.StatusOK)
		var body getConversationBody // fresh decode: an absent field must not inherit a stale value
		decodeJSON(t, rr, &body)
		if ok(body) {
			return body
		}
		if time.Now().After(deadline) {
			t.Fatalf("conversation never reached the expected state; last body %s", rr.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestHandleGetConversation_ReportsActiveStream: while a stream for the
// (flow, conversation) is live — once its question is stored, which the
// worker does right after prepareTurn — GET conversation carries its id as
// activeStreamId so a reloaded client can re-attach, next to a history that
// holds the question; a conversation with no stream reports none. Once the
// stream finished and its answer is stored the id is gone. The
// retention-window eviction is covered at the service level
// (TestActiveStreamFor) rather than by sleeping out the two-minute retention
// here.
func TestHandleGetConversation_ReportsActiveStream(t *testing.T) {
	// The router's settings store reads the OS config dir: point it at a temp
	// dir so "Save conversation history" has its default (on) — the stream
	// is only reported once its question is stored — and the real settings
	// file is never read.
	cfgDir := t.TempDir()
	t.Setenv("APPDATA", cfgDir)
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	t.Setenv("HOME", cfgDir)
	p := &blockingStreamProvider{FakeProvider: &testutil.FakeProvider{}, release: make(chan struct{})}
	defer p.closeOnce.Do(func() { close(p.release) })
	ai.RegisterProvider("f21-blocking", func(string) ai.Provider { return p })

	fs, err := filesystem.NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("local storage: %v", err)
	}
	rt := newTestRouter(fs, true)
	doc := &interfaces.FlowDocument{ID: "f-f21", Name: "t", OwnerID: "u1"}
	if err := rt.security.Backend.SaveFlow(context.Background(), doc); err != nil {
		t.Fatalf("seed flow: %v", err)
	}
	auth := jwtBearer(t, rt, "u1", "u1@example.com")

	const convID = "11111111-2222-4333-8444-555555555555"
	const clientStreamID = "99999999-9999-4999-8999-999999999999"

	// No stream yet: messages come back (wrapped), no activeStreamId.
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/get",
		auth, map[string]string{"flowId": "f-f21", "provider": convID})
	checkStatus(t, rr, http.StatusOK)
	var body getConversationBody
	decodeJSON(t, rr, &body)
	if body.ActiveStreamID != "" {
		t.Errorf("activeStreamId = %q before any stream, want empty", body.ActiveStreamID)
	}

	// Start a stream on the conversation; it blocks mid-answer.
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/stream", auth, map[string]any{
		"flowId": "f-f21", "provider": "f21-blocking", "userMessage": "long question",
		"conversationId": convID, "clientStreamId": clientStreamID,
	})
	checkStatus(t, rr, http.StatusOK)
	if strings.TrimSpace(rr.Body.String()) != `"`+clientStreamID+`"` {
		t.Fatalf("stream create replied %s", rr.Body.String())
	}

	// While live (and once the question is stored), GET conversation reports
	// the stream, next to the stored question.
	body = pollConversation(t, rt, auth, convID, func(b getConversationBody) bool { return b.ActiveStreamID != "" })
	if body.ActiveStreamID != clientStreamID {
		t.Errorf("activeStreamId = %q while the stream is live, want %q", body.ActiveStreamID, clientStreamID)
	}
	if len(body.Messages) != 1 || !strings.Contains(string(body.Messages[0]), "long question") {
		t.Errorf("messages reported with the stream = %s, want the stored question", body.Messages)
	}

	// Another conversation of the same caller reports no stream.
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/get",
		auth, map[string]string{"flowId": "f-f21", "provider": "22222222-3333-4433-8543-555555555556"})
	checkStatus(t, rr, http.StatusOK)
	var other getConversationBody // fresh decode: absent fields must not inherit
	decodeJSON(t, rr, &other)
	if other.ActiveStreamID != "" {
		t.Errorf("activeStreamId for another conversation = %q, want empty", other.ActiveStreamID)
	}

	// Let the stream finish: once its reply is stored with the history the
	// finished stream is no longer reported (a resume would duplicate it).
	// markDone lands before the cleanup stores the answer, so poll rather
	// than read once.
	p.closeOnce.Do(func() { close(p.release) })
	body = pollConversation(t, rt, auth, convID, func(b getConversationBody) bool { return b.ActiveStreamID == "" })
	if len(body.Messages) != 2 || !strings.Contains(string(body.Messages[1]), "partial") {
		t.Errorf("messages after finish = %s, want the question and the stored answer", body.Messages)
	}
}
