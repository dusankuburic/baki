package api

import (
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

func TestHandleExportConversation_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/chat/export")
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
