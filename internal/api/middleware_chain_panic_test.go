package api

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"pad-analyzer/internal/config"
)

// TestBuildHandler_HandlerPanicIsCountedAsA500: a handler panic must reach the
// Metrics (and AccessLog) layers as a 500. Recovery used to sit OUTSIDE both,
// and neither defers its bookkeeping, so the panic unwound straight past them:
// the request produced no access-log line and no http_requests_total{5xx}
// sample. 5xx alerting was blind to the one failure mode it exists for.
func TestBuildHandler_HandlerPanicIsCountedAsA500(t *testing.T) {
	rt := newJWTTestRouter(t)
	rt.mux.Get("/api/panic-probe", func(http.ResponseWriter, *http.Request) {
		panic("handler exploded")
	})
	cfg := config.Default() // local mode: no rate limiters, no Redis
	h, limiters := BuildHandler(rt, cfg, nil)
	for _, l := range limiters {
		l.Stop()
	}

	labels := map[string]string{"method": "GET", "status": "500"}
	before := counterSum(t, "http_requests_total", labels)

	req := httptest.NewRequest(http.MethodGet, "/api/panic-probe", nil)
	req.Header.Set("Authorization", jwtBearer(t, rt, "u-1", "u@example.com"))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("expected 500, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	if after := counterSum(t, "http_requests_total", labels); after != before+1 {
		t.Errorf("http_requests_total{status=500} went %v → %v; the Metrics layer never saw the panicking request", before, after)
	}
}
