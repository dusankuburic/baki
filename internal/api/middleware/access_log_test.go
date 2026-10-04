package middleware

import (
	"bytes"
	"context"
	"fmt"
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

	handler := AccessLog(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	handler := AccessLog(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
	handler := AccessLog(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	handler := AccessLog(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

	handler := AccessLog(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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

// TestLevelFor_DeliberateRefusalsAreWarn pins the 5xx split: 501 and 503 are
// the service refusing by design (unconfigured feature, connection cap,
// dependency declared unavailable) and log at WARN; every other 5xx is a
// failure and logs at ERROR.
func TestLevelFor_DeliberateRefusalsAreWarn(t *testing.T) {
	cases := []struct {
		path   string
		status int
		want   string
	}{
		{"/api/x", http.StatusOK, "info"},
		{"/api/x", http.StatusNotFound, "warn"},
		{"/api/x", http.StatusTooManyRequests, "warn"},
		{"/api/x", http.StatusInternalServerError, "error"},
		{"/api/x", http.StatusNotImplemented, "warn"},
		{"/api/x", http.StatusBadGateway, "error"},
		{"/api/integrations/ci", http.StatusServiceUnavailable, "warn"},
		{"/api/x", http.StatusGatewayTimeout, "error"},
		{"/api/x", http.StatusHTTPVersionNotSupported, "error"},
		// Probes stay at debug whatever they answer.
		{"/readyz", http.StatusServiceUnavailable, "debug"},
	}
	for _, c := range cases {
		if got := levelFor(c.path, c.status); got != c.want {
			t.Errorf("levelFor(%q, %d) = %q, want %q", c.path, c.status, got, c.want)
		}
	}
}

// TestAccessLog_UnconfiguredWebhook503IsNotAnError drives the real middleware
// with the response POST /api/integrations/ci gives when
// PAD_CI_WEBHOOK_SECRET is unset. The handler deliberately writes it without
// an ERROR line (an unauthenticated caller can repeat it at will); the access
// line must not put one back. A 500 still logs at ERROR.
func TestAccessLog_UnconfiguredWebhook503IsNotAnError(t *testing.T) {
	logs := captureSlog(t)
	serve := func(status int) string {
		logs.Reset()
		handler := AccessLog(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(status)
		}))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/api/integrations/ci", nil))
		return logs.String()
	}

	out := serve(http.StatusServiceUnavailable)
	if strings.Contains(out, "level=ERROR") {
		t.Errorf("a 503 refusal was logged at ERROR:\n%s", out)
	}
	if !strings.Contains(out, "level=WARN") || !strings.Contains(out, "status=503") {
		t.Errorf("expected the 503 access line at WARN:\n%s", out)
	}

	if out := serve(http.StatusInternalServerError); !strings.Contains(out, "level=ERROR") {
		t.Errorf("a 500 must still log at ERROR:\n%s", out)
	}
}

func TestRequestIDFromContext_EmptyWhenMiddlewareNotApplied(t *testing.T) {
	if got := RequestIDFromContext(context.Background()); got != "" {
		t.Errorf("expected empty when middleware wasn't applied, got %q", got)
	}
}

// TestRedactPath_InviteToken guards the one route whose PATH segment is a live
// credential. The access log and the panic reporter both write the request
// path; before redaction, accepting an invite republished the emailed
// single-use token into log storage.
func TestRedactPath_InviteToken(t *testing.T) {
	cases := []struct{ in, want string }{
		{"/api/invites/s3cret-token/accept", "/api/invites/[redacted]/accept"},
		{"/api/invites/s3cret-token", "/api/invites/[redacted]"},
		// Shape preserved, nothing else touched.
		{"/api/invites/", "/api/invites/"},
		{"/api/auth/login", "/api/auth/login"},
		{"/api/library/abc123", "/api/library/abc123"},
		{"/", "/"},
	}
	for _, c := range cases {
		if got := redactPath(c.in); got != c.want {
			t.Errorf("redactPath(%q) = %q, want %q", c.in, got, c.want)
		}
	}
	// The secret must not survive anywhere in the output.
	if got := redactPath("/api/invites/s3cret-token/accept"); strings.Contains(got, "s3cret-token") {
		t.Errorf("token leaked through redactPath: %q", got)
	}
}

// TestAccessLog_RedactsTheInviteTokenOfARequestRefusedBeforeTheRouter: the
// access log redacted r.URL.Path, which is canonical only once the router has
// rewritten it in place. A request the rate limiter refused kept its raw
// spelling, and every spelling but the canonical one — the documented /api/v1
// alias included — logged the live invite credential verbatim.
func TestAccessLog_RedactsTheInviteTokenOfARequestRefusedBeforeTheRouter(t *testing.T) {
	logs := captureSlog(t)
	rl := NewRateLimiter(0, 0, nil)
	defer rl.Stop()
	handler := AccessLog(nil)(rl.Limit(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("the limiter let POST %s through", r.URL.Path)
	})))
	for _, target := range []string{
		"/api/invites/s3cret-a/accept",
		"/api/v1/invites/s3cret-b/accept",
		"/api//invites/s3cret-c/accept",
		"/api/./invites/s3cret-d/accept",
	} {
		logs.Reset()
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, httptest.NewRequest(http.MethodPost, target, nil))
		if rr.Code != http.StatusTooManyRequests {
			t.Fatalf("POST %s = %d, want the limiter's 429", target, rr.Code)
		}
		out := logs.String()
		if strings.Contains(out, "s3cret") {
			t.Errorf("POST %s refused before the router logged the invite token:\n%s", target, out)
		}
		if !strings.Contains(out, "path=/api/invites/[redacted]/accept") {
			t.Errorf("POST %s: expected the redacted canonical path on the access line:\n%s", target, out)
		}
	}
}

// TestAccessLog_DoesNotReparseTrustedProxiesPerRequest: the access line's
// remote_ip must not come from ClientIP, which parses every trusted-proxy entry
// (a net.ParseCIDR per CIDR) on every request. The list is parsed once when
// the middleware is built, so a request's allocations must not grow with the
// length of the list. The peer is outside every entry, so the whole list is
// walked either way.
func TestAccessLog_DoesNotReparseTrustedProxiesPerRequest(t *testing.T) {
	_ = captureSlog(t)
	allocs := func(proxies []string) float64 {
		handler := AccessLog(proxies)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusNoContent)
		}))
		req := httptest.NewRequest(http.MethodGet, "/api/something", nil)
		req.RemoteAddr = "203.0.113.5:1234"
		req.Header.Set(RequestIDHeader, "fixed-id")
		req.Header.Set("X-Forwarded-For", "198.51.100.1")
		return testing.AllocsPerRun(200, func() {
			handler.ServeHTTP(httptest.NewRecorder(), req)
		})
	}
	many := make([]string, 0, 64)
	for i := 0; i < 64; i++ {
		many = append(many, fmt.Sprintf("10.%d.0.0/16", i))
	}
	one, sixtyFour := allocs([]string{"10.0.0.0/16"}), allocs(many)
	// A small allowance absorbs allocation-count noise between runs (19 vs 20
	// was seen); re-parsing the list per request costs hundreds (24 → 282).
	if sixtyFour > one+4 {
		t.Errorf("allocations per request grow with the trusted-proxy list: %v with 1 entry, %v with 64", one, sixtyFour)
	}
}
