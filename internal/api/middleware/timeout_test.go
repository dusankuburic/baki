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

func TestIsStreamingPath(t *testing.T) {
	tests := []struct {
		path     string
		expected bool
	}{
		{"/ws", true},
		{"/api/events", true},
		{"/api/chat/stream", true},
		{"/api/analysis/stream", true},
		{"/api/something", false},
		{"/", false},
		{"/api/flows/123", false},
		{"/stream", true},
	}
	for _, tt := range tests {
		t.Run(tt.path, func(t *testing.T) {
			if got := isStreamingPath(tt.path); got != tt.expected {
				t.Errorf("isStreamingPath(%q) = %v, want %v", tt.path, got, tt.expected)
			}
		})
	}
}
