package api

import (
	"net/http"
	"testing"
)

func TestHandleCompareCurrentWith_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/export/compare")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleExportMarkdown_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/export/markdown")
	checkStatus(t, rr, http.StatusBadRequest)
}

func TestHandleExportPDF_BadBodyReturns400(t *testing.T) {
	rt := newTestRouter()
	rr := newBadBodyRequest(t, rt, http.MethodPost, "/api/export/pdf")
	checkStatus(t, rr, http.StatusBadRequest)
}
