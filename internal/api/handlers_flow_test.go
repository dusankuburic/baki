package api

import (
	"net/http"
	"testing"
)

// --- Bad-body 400 tests ---

func TestHandleLoadFlowFromPath_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/flow/load-path")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleLoadFlowFolder_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/flow/load-folder")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleRemoveRecentFile_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/flow/remove-recent")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleRevealInFileManager_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/flow/reveal")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleSearchFlow_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/flow/search")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleReadSourceFiles_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/flow/read-sources")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleOnFileOpenFromSystem_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/flow/open-from-system")
	checkStatus(t, rr, http.StatusBadRequest)
}

// --- Fire-and-forget endpoint (nil-checked in App, not Guard-protected → 200) ---

func TestHandleOnFileOpenFromSystem_ValidBodyReturnsOK(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodPost, "/api/flow/open-from-system", map[string]string{"path": "/tmp/test.pad"})
	checkStatus(t, rr, http.StatusOK)
}

// --- No-body endpoints (services nil → logger.Guard catches panic → 500) ---

func TestHandleRecentFiles_UninitializedAppReturns500(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodGet, "/api/flow/recent", nil)
	// Services are nil without App.Init(); logger.Guard converts the nil-deref to an error.
	checkStatus(t, rr, http.StatusInternalServerError)
}

func TestHandleClearRecentFiles_UninitializedAppReturns500(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodPost, "/api/flow/clear-recent", nil)
	checkStatus(t, rr, http.StatusInternalServerError)
}

func TestHandleGetSourceFiles_UninitializedAppReturns500(t *testing.T) {
	rt := newTestRouter()
	rr := doRequest(t, rt, http.MethodGet, "/api/flow/source-files", nil)
	checkStatus(t, rr, http.StatusInternalServerError)
}
