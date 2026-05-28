package api

import (
	"net/http"
	"testing"
)

// --- Bad-body 400 tests ---

func TestHandleStreamChatMessage_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/chat/stream")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleBeginStream_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/chat/begin")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleCancelStream_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/chat/cancel")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleGetConversation_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/chat/get")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleSaveConversation_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/chat/save")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleClearConversation_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/chat/clear")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleExportConversation_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/chat/export")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandlePreviewContext_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/chat/preview-context")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleGetSuggestedPrompts_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/chat/suggested-prompts")
	checkStatus(t, rr, http.StatusBadRequest)
}

// --- Fire-and-forget endpoints (nil-checked in App, not Guard-protected → 200) ---

func TestHandleBeginStream_ValidBodyReturnsOK(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodPost, "/api/chat/begin", map[string]string{"streamId": "s1"})
	checkStatus(t, rr, http.StatusOK)
}

func TestHandleCancelStream_ValidBodyReturnsOK(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodPost, "/api/chat/cancel", map[string]string{"streamId": "s1"})
	checkStatus(t, rr, http.StatusOK)
}

// --- No-body endpoint (Guard-protected → 500 with nil app services) ---

func TestHandleGetDemoRemaining_UninitializedAppReturns500(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodGet, "/api/chat/demo-remaining", nil)
	checkStatus(t, rr, http.StatusInternalServerError)
}
