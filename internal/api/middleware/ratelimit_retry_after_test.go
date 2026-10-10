package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A refusal carries Retry-After: the whole seconds one token takes to refill, at least 1 (bakicli report waits it).
func TestRateLimiter_RefusalCarriesRetryAfter(t *testing.T) {
	for _, c := range []struct {
		rps  float64
		want string
	}{{2, "1"}, {0.2, "5"}, {0, "1"}} {
		rl := NewRateLimiter(c.rps, 1, nil)
		h := rl.Limit(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }))
		keyed := rl.LimitByKey(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(http.StatusOK) }),
			func(*http.Request) string { return "k" })
		for name, handler := range map[string]http.Handler{"Limit": h, "LimitByKey": keyed} {
			var rec *httptest.ResponseRecorder
			for range 2 {
				req := httptest.NewRequest(http.MethodGet, "/", nil)
				req.RemoteAddr = "8.8.8.8:1234"
				rec = httptest.NewRecorder()
				handler.ServeHTTP(rec, req)
			}
			if rec.Code != http.StatusTooManyRequests || rec.Header().Get("Retry-After") != c.want {
				t.Errorf("%s at %v rps: %d with Retry-After %q, want 429 with %q", name, c.rps, rec.Code, rec.Header().Get("Retry-After"), c.want)
			}
		}
		rl.Stop()
	}
}
