package api

import (
	"encoding/json"
	"net/http"
	"testing"
)

// --- Bad-body 400 tests ---

func TestHandleStreamChatMessage_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/chat/stream")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleBeginStream_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/chat/begin")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleCancelStream_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/chat/cancel")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleGetConversation_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/chat/get")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleSaveConversation_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/chat/save")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleClearConversation_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/chat/clear")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandlePreviewContext_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/chat/preview-context")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleGetSuggestedPrompts_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/chat/suggested-prompts")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleCancelStream_ValidBodyReturnsOK(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodPost, "/api/chat/cancel", map[string]string{"streamId": "s1"})
	checkStatus(t, rr, http.StatusOK)
}

func TestHandleResumeStream_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/chat/resume")
	checkStatus(t, rr, http.StatusBadRequest)
}

// Resuming an unknown stream id reaches the handler (route is registered) and
// returns 404 — a missing route would surface as 405/404-from-router instead.
func TestHandleResumeStream_UnknownIDReturns404(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodPost, "/api/chat/resume", map[string]string{"id": "nope"})
	checkStatus(t, rr, http.StatusNotFound)
}

// --- fix-decision endpoint ---

func TestHandleFixDecision_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/chat/fix-decision")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleFixDecision_MissingFieldsReturn400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodPost, "/api/chat/fix-decision", map[string]string{"streamId": "s1"})
	checkStatus(t, rr, http.StatusBadRequest)
}

// An unknown proposal on a locally-owned stream maps ErrNoPendingFix to 404.
func TestHandleFixDecision_UnknownProposalReturns404(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodPost, "/api/chat/fix-decision", map[string]interface{}{
		"streamId": "s1", "proposalId": "nope", "approved": true,
	})
	checkStatus(t, rr, http.StatusNotFound)
}

// --- conversation wire contract ---

// /api/chat/get answers with the message list as a BARE JSON array — the shape
// the frontend parses (frontend/src/lib/chatMessage.ts parseConversationPayload).
// The client once read `.messages` off this array, restored nothing on every
// reload, and its next save then replaced the stored history with just the new
// turn. This pins the wire shape end to end: save, then read it back.
func TestChatGet_ReturnsTheSavedMessagesAsABareArray(t *testing.T) {
	// The test router's settings store reads the machine's real settings file
	// (os.UserConfigDir); a developer who turned "Save conversation history"
	// off there would see the save silently dropped. Point the config dir at a
	// temp dir so the defaults apply — and the real file is never touched.
	cfgDir := t.TempDir()
	t.Setenv("APPDATA", cfgDir)
	t.Setenv("XDG_CONFIG_HOME", cfgDir)
	t.Setenv("HOME", cfgDir)
	rt := newJWTTestRouter(t)
	seedFlow(t, rt, "flow-a", "alice")
	tok := jwtBearer(t, rt, "alice", "alice@example.com")
	key := "11111111-1111-4111-8111-111111111111"

	save := doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/save", tok, map[string]any{
		"flowId": "flow-a", "provider": key,
		"messages": []map[string]any{
			{"id": "m1", "role": "user", "content": "hi", "timestamp": "2024-01-01T00:00:00Z"},
			{"id": "m2", "role": "assistant", "content": "hello", "timestamp": "2024-01-01T00:00:01Z"},
		},
	})
	checkStatus(t, save, http.StatusOK)

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/get", tok, map[string]any{"flowId": "flow-a", "provider": key})
	checkStatus(t, rr, http.StatusOK)
	var got []map[string]any
	if err := json.Unmarshal(rr.Body.Bytes(), &got); err != nil {
		t.Fatalf("want a JSON array of messages, got %s (%v)", rr.Body.String(), err)
	}
	if len(got) != 2 || got[0]["id"] != "m1" || got[1]["content"] != "hello" {
		t.Errorf("round trip lost messages: %s", rr.Body.String())
	}
}
