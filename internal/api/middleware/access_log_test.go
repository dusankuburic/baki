package middleware

import (
	"bytes"
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// captureSlog replaces the default slog logger with a buffer-backed handler
// so a test can assert on log output. Not safe under t.Parallel().
func captureSlog(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestAccessLog_GeneratesRequestIDWhenMissing(t *testing.T) {
	_ = captureSlog(t)

	handler := AccessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The handler should see the generated ID on the context.
		if RequestIDFromContext(r.Context()) == "" {
			t.Error("expected non-empty request ID on context")
		}
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/something", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if got := rr.Header().Get(RequestIDHeader); got == "" {
		t.Error("expected X-Request-ID echoed in response")
	}
}

func TestAccessLog_PreservesUpstreamRequestID(t *testing.T) {
	_ = captureSlog(t)
	const upstream = "lb-deadbeef-1234"

	var seen string
	handler := AccessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/something", nil)
	req.Header.Set(RequestIDHeader, upstream)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if seen != upstream {
		t.Errorf("context request_id: got %q, want %q (upstream ID must propagate)", seen, upstream)
	}
	if rr.Header().Get(RequestIDHeader) != upstream {
		t.Errorf("response header: got %q, want %q", rr.Header().Get(RequestIDHeader), upstream)
	}
}

func TestAccessLog_OverlongUpstreamIDReplacedToBoundLogLineSize(t *testing.T) {
	_ = captureSlog(t)
	tooLong := strings.Repeat("a", 200)

	var seen string
	handler := AccessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = RequestIDFromContext(r.Context())
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/api/something", nil)
	req.Header.Set(RequestIDHeader, tooLong)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if seen == tooLong {
		t.Error("expected the overlong ID to be replaced by a generated one")
	}
	if seen == "" {
		t.Error("expected a generated ID, got empty")
	}
}

func TestAccessLog_EmitsStructuredLineWithStatusAndLatency(t *testing.T) {
	logs := captureSlog(t)

	handler := AccessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusTeapot)
		_, _ = w.Write([]byte("hello"))
	}))

	req := httptest.NewRequest(http.MethodPost, "/api/test", nil)
	req.RemoteAddr = "8.8.8.8:12345"
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	out := logs.String()
	for _, expected := range []string{
		`msg=access`,
		`status=418`,
		`method=POST`,
		`path=/api/test`,
		`remote_ip=8.8.8.8`,
		`bytes=5`,
	} {
		if !strings.Contains(out, expected) {
			t.Errorf("access log missing %q. Got: %s", expected, out)
		}
	}
}

func TestAccessLog_LevelDemotionForHealthProbes(t *testing.T) {
	logs := captureSlog(t)

	handler := AccessLog(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))

	req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	out := logs.String()
	if !strings.Contains(out, "level=DEBUG") {
		t.Errorf("expected health probe logged at DEBUG (avoids drowning access log). Got: %s", out)
	}
}

func TestRequestIDFromContext_EmptyWhenMiddlewareNotApplied(t *testing.T) {
	if got := RequestIDFromContext(context.Background()); got != "" {
		t.Errorf("expected empty when middleware wasn't applied, got %q", got)
	}
}
