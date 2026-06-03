package api

import (
	"net/http"
	"testing"
)

// --- Bad-body 400 tests ---


func TestHandleGetSourceFiles_NoDocReturnsOK(t *testing.T) {
	rt := newTestRouter(nil, false)
	rr := doRequest(t, rt, http.MethodGet, "/api/flow/source-files", nil)
	// GetSourceFiles returns nil gracefully when no document is loaded.
	checkStatus(t, rr, http.StatusOK)
}
