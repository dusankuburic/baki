package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// --- Bad-body 400 tests ---

func TestHandleGetSourceFiles_NoDocReturnsOK(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodGet, "/api/flow/source-files", nil)
	// GetSourceFiles returns nil gracefully when no document is loaded.
	checkStatus(t, rr, http.StatusOK)
}

// TestHandleReimport_MalformedBodyReturns400 guards L (handlers_flow): a
// malformed JSON body on the re-import endpoint previously had its decode
// error swallowed, leaving req.FlowID empty and producing a misleading
// "flow not found" instead of a clean 400.
func TestHandleReimport_MalformedBodyReturns400(t *testing.T) {
	rt := newTestRouter(nil, false) // local mode (re-import is cloud-disabled)
	req := httptest.NewRequest(http.MethodPost, "/api/flow/reimport", strings.NewReader("{not valid json"))
	req.Header.Set("Authorization", "Bearer "+testToken)
	rr := httptest.NewRecorder()
	rt.ServeHTTP(rr, req)
	checkStatus(t, rr, http.StatusBadRequest)
}
