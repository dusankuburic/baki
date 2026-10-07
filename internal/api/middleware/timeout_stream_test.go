package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"pad-analyzer/internal/config"
)

// TestRequestTimeout_StreamRoutesGetAnOuterDeadline: chat/analysis streams
// (*/stream) are exempt from the per-request deadline because a model reply can
// legitimately take minutes — but "exempt" used to mean "no deadline at all",
// so a stalled upstream held the goroutine, the RLS transaction, and the
// pooled connection forever. They now get a generous outer deadline.
// /ws and /api/events are long-lived by design and stay unbounded.
func TestRequestTimeout_StreamRoutesGetAnOuterDeadline(t *testing.T) {
	cases := []struct {
		path         string
		wantDeadline bool
	}{
		{"/api/chat/stream", true},
		{"/api/analysis/stream", true},
		{"/ws", false},
		{"/api/events", false},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			var got time.Time
			var has bool
			h := RequestTimeout(30*time.Second, config.RoutePolicyLegacy)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, has = r.Context().Deadline()
				w.WriteHeader(http.StatusOK)
			}))
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, c.path, nil))
			if has != c.wantDeadline {
				t.Fatalf("deadline present = %v, want %v", has, c.wantDeadline)
			}
			if !has {
				return
			}
			remaining := time.Until(got)
			// Generous (well past the 30s request timeout) but finite.
			if remaining < 5*time.Minute || remaining > StreamRequestTimeout+time.Minute {
				t.Errorf("stream deadline %v from now; want ≈ %v", remaining.Round(time.Second), StreamRequestTimeout)
			}
		})
	}
}
