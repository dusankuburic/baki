package middleware

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRequestTimeout_StreamingPathsBypass(t *testing.T) {
	streamingPaths := []string{"/ws", "/api/events", "/api/chat/stream"}
	for _, p := range streamingPaths {
		t.Run(p, func(t *testing.T) {
			var ctxCancelled bool
			handler := RequestTimeout(50 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				select {
				case <-r.Context().Done():
					ctxCancelled = true
				case <-time.After(100 * time.Millisecond):
				}
				w.WriteHeader(http.StatusOK)
			}))

			req := httptest.NewRequest(http.MethodGet, p, nil)
			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if ctxCancelled {
				t.Error("streaming path should bypass timeout, context was cancelled")
			}
			if rr.Code != http.StatusOK {
				t.Errorf("expected 200, got %d", rr.Code)
			}
		})
	}
}

func TestRequestTimeout_NormalPathGetsDeadline(t *testing.T) {
	handler := RequestTimeout(20 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadline, ok := r.Context().Value(0).(time.Time)
		_ = deadline
		_ = ok
		// The context should have a deadline set
		_, hasDeadline := r.Context().Deadline()
		if !hasDeadline {
			t.Error("expected context to have a deadline")
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/something", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("expected 200, got %d", rr.Code)
	}
}

func TestRequestTimeout_ContextCancelsAfterDuration(t *testing.T) {
	handler := RequestTimeout(10 * time.Millisecond)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			t.Error("context cancelled before handler finished")
		case <-time.After(5 * time.Millisecond):
		}
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/something", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
}

func TestRequestTimeout_ContextAlreadyCancelledPropagates(t *testing.T) {
	handler := RequestTimeout(time.Hour)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Context().Err() == nil {
			t.Error("expected context to be already cancelled")
		}
	}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	req := httptest.NewRequest(http.MethodGet, "/api/something", nil).WithContext(ctx)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
}

// TestRequestTimeout_BulkKnowledgePathsGetLongDeadline: the knowledge-base
// bulk routes (upload, re-index) run a sequence of embedding calls and get
// the bounded-long BulkRequestTimeout; the org's other knowledge routes
// keep the ordinary per-request deadline.
func TestRequestTimeout_BulkKnowledgePathsGetLongDeadline(t *testing.T) {
	cases := []struct {
		path string
		bulk bool
	}{
		{"/api/orgs/o/knowledge/reindex", true},
		{"/api/orgs/o/knowledge/upload", true},
		{"/api/orgs/o/knowledge", false},
		{"/api/orgs/o/knowledge/doc-1", false},
		{"/api/orgs/o/knowledge/upload/extra", false},
	}
	for _, c := range cases {
		t.Run(c.path, func(t *testing.T) {
			var got time.Time
			var has bool
			h := RequestTimeout(30 * time.Second)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				got, has = r.Context().Deadline()
				w.WriteHeader(http.StatusOK)
			}))
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, c.path, nil))
			if !has {
				t.Fatal("no deadline: every knowledge route is bounded")
			}
			remaining := time.Until(got)
			if c.bulk {
				if remaining < time.Minute || remaining > BulkRequestTimeout+time.Minute {
					t.Errorf("bulk deadline %v from now; want ≈ %v", remaining.Round(time.Second), BulkRequestTimeout)
				}
			} else if remaining > 30*time.Second+time.Second {
				t.Errorf("ordinary deadline %v from now; want ≤ 30s", remaining.Round(time.Second))
			}
			if isBulkPath(c.path) != c.bulk {
				t.Errorf("isBulkPath(%q) = %v, want %v", c.path, !c.bulk, c.bulk)
			}
		})
	}
}

func TestIsStreamingPath(t *testing.T) {
	tests := []struct {
		path     string
		expected bool
	}{
		// Bounded streams (*/stream) get the generous outer deadline …
		{"/api/chat/stream", true},
		{"/api/analysis/stream", true},
		{"/stream", true},
		// … open-ended sessions are long-lived, not streaming (no deadline).
		{"/ws", false},
		{"/api/events", false},
		{"/api/something", false},
		{"/", false},
		{"/api/flows/123", false},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := isStreamingPath(tt.path); got != tt.expected {
				t.Errorf("isStreamingPath(%q) = %v, want %v", tt.path, got, tt.expected)
			}
		})
	}
	for _, p := range []string{"/ws", "/api/events"} {
		if !isLongLivedPath(p) {
			t.Errorf("isLongLivedPath(%q) = false; WebSocket/SSE sessions must stay unbounded", p)
		}
	}
	if isLongLivedPath("/api/chat/stream") {
		t.Error("a chat stream is bounded, not long-lived")
	}
}
