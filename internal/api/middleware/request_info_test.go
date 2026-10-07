package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/google/uuid"
	"go.opentelemetry.io/otel/trace"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/platform/reqinfo"
	"pad-core/logger"
)

// serveInfo sends req through RequestInfo and returns the Info the inner handler saw and the response.
func serveInfo(t *testing.T, proxies []string, req *http.Request) (*reqinfo.Info, *httptest.ResponseRecorder) {
	t.Helper()
	var seen *reqinfo.Info
	h := RequestInfo(proxies)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seen = reqinfo.From(r.Context())
		if got := RequestIDFromContext(r.Context()); seen != nil && got != seen.ID {
			t.Errorf("RequestIDFromContext = %q, want the Info's %q", got, seen.ID)
		}
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	if seen == nil {
		t.Fatal("the handler saw no reqinfo.Info")
	}
	return seen, rr
}

func TestRequestInfo_KeepsTheInboundIDAndEchoesIt(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	req.Header.Set(RequestIDHeader, "  lb-deadbeef-1234 ")
	info, rr := serveInfo(t, nil, req)
	if info.ID != "lb-deadbeef-1234" {
		t.Errorf("Info.ID = %q, want the trimmed inbound id", info.ID)
	}
	if got := rr.Header().Get(RequestIDHeader); got != info.ID {
		t.Errorf("response %s = %q, want %q", RequestIDHeader, got, info.ID)
	}
}

func TestRequestInfo_GeneratesAnIDWhenMissingOrTooLong(t *testing.T) {
	for name, header := range map[string]string{
		"absent":   "",
		"blank":    "   ",
		"too long": strings.Repeat("a", maxRequestIDLen+1),
	} {
		req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
		if header != "" {
			req.Header.Set(RequestIDHeader, header)
		}
		info, rr := serveInfo(t, nil, req)
		if _, err := uuid.Parse(info.ID); err != nil {
			t.Errorf("%s: Info.ID = %q, want a generated UUID", name, info.ID)
		}
		if got := rr.Header().Get(RequestIDHeader); got != info.ID {
			t.Errorf("%s: response %s = %q, want %q", name, RequestIDHeader, got, info.ID)
		}
	}

	req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	req.Header.Set(RequestIDHeader, strings.Repeat("b", maxRequestIDLen))
	if info, _ := serveInfo(t, nil, req); info.ID != strings.Repeat("b", maxRequestIDLen) {
		t.Errorf("an id of exactly %d bytes must be kept, got %q", maxRequestIDLen, info.ID)
	}
}

func TestRequestInfo_ClientAndPeerIP(t *testing.T) {
	proxies := []string{"10.0.0.0/8"}

	// Behind the trusted proxy: the client is the address the proxy appended, the peer is the proxy.
	req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	req.RemoteAddr = "10.1.2.3:41000"
	req.Header.Set("X-Forwarded-For", "203.0.113.66, 198.51.100.7")
	info, _ := serveInfo(t, proxies, req)
	if info.ClientIP != "198.51.100.7" {
		t.Errorf("ClientIP = %q, want the rightmost X-Forwarded-For entry", info.ClientIP)
	}
	if info.PeerIP != "10.1.2.3" {
		t.Errorf("PeerIP = %q, want the proxy's address", info.PeerIP)
	}

	// A peer outside the trusted set cannot claim another address.
	req = httptest.NewRequest(http.MethodGet, "/api/x", nil)
	req.RemoteAddr = "[2001:db8::1]:41000"
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	info, _ = serveInfo(t, proxies, req)
	if info.ClientIP != "2001:db8::1" || info.PeerIP != "2001:db8::1" {
		t.Errorf("untrusted peer: ClientIP %q, PeerIP %q, want both 2001:db8::1", info.ClientIP, info.PeerIP)
	}
}

func TestRequestInfo_BoundsTheUserAgent(t *testing.T) {
	req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	req.Header.Set("User-Agent", "curl/8.5.0")
	if info, _ := serveInfo(t, nil, req); info.UserAgent != "curl/8.5.0" {
		t.Errorf("UserAgent = %q", info.UserAgent)
	}

	// A multi-byte rune straddling the bound is dropped whole, not split.
	long := strings.Repeat("a", maxUserAgentLen-1) + "é" + strings.Repeat("z", 100)
	req.Header.Set("User-Agent", long)
	info, _ := serveInfo(t, nil, req)
	if len(info.UserAgent) > maxUserAgentLen || !utf8.ValidString(info.UserAgent) {
		t.Errorf("UserAgent is %d bytes (valid UTF-8: %v), want at most %d", len(info.UserAgent), utf8.ValidString(info.UserAgent), maxUserAgentLen)
	}
	if !strings.HasPrefix(long, info.UserAgent) || len(info.UserAgent) != maxUserAgentLen-1 {
		t.Errorf("UserAgent should be the first %d bytes, got %d", maxUserAgentLen-1, len(info.UserAgent))
	}
}

func TestRequestInfo_ReusesAnInstalledInfo(t *testing.T) {
	outer := &reqinfo.Info{ID: "outer-id"}
	req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	req.Header.Set(RequestIDHeader, "inbound")
	req = req.WithContext(reqinfo.With(req.Context(), outer))
	info, rr := serveInfo(t, nil, req)
	if info != outer {
		t.Fatal("RequestInfo replaced the Info the context already carried")
	}
	if got := rr.Header().Get(RequestIDHeader); got != "outer-id" {
		t.Errorf("response %s = %q, want the installed Info's id", RequestIDHeader, got)
	}
}

// TestAccessLog_ReadsTheActorRecordedFurtherIn is the user_id fix: authentication records the actor on the
// shared Info, which the access log, outside it, reads once the handler has returned.
func TestAccessLog_ReadsTheActorRecordedFurtherIn(t *testing.T) {
	logs := captureSlog(t)
	h := RequestInfo([]string{"10.0.0.0/8"})(AccessLog([]string{"10.0.0.0/8"})(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// What jwtAuth does further in: the claims go on a context only the layers inside it see, the actor on
		// the shared Info.
		reqinfo.From(r.Context()).SetActor(&auth.Actor{Kind: auth.ActorUser, UserID: "u-42"})
		w.WriteHeader(http.StatusNoContent)
	})))
	req := httptest.NewRequest(http.MethodGet, "/api/x", nil)
	req.RemoteAddr = "10.9.9.9:5000"
	req.Header.Set("X-Forwarded-For", "198.51.100.7")
	req.Header.Set(RequestIDHeader, "req-abc")
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)

	out := logs.String()
	for _, want := range []string{"msg=access", "request_id=req-abc", "user_id=u-42", "remote_ip=198.51.100.7", "status=204"} {
		if !strings.Contains(out, want) {
			t.Errorf("access line missing %q:\n%s", want, out)
		}
	}
	if got := rr.Header().Values(RequestIDHeader); len(got) != 1 || got[0] != "req-abc" {
		t.Errorf("response %s = %q, want it once", RequestIDHeader, got)
	}
}

func TestLogAttrs_RequestActorAndTrace(t *testing.T) {
	logs := captureSlog(t)
	logger.SetContextAttrs(LogAttrs)
	t.Cleanup(func() { logger.SetContextAttrs(nil) })

	tid, _ := trace.TraceIDFromHex("4bf92f3577b34da6a3ce929d0e0e4736")
	sid, _ := trace.SpanIDFromHex("00f067aa0ba902b7")
	spanCtx := trace.ContextWithSpanContext(context.Background(),
		trace.NewSpanContext(trace.SpanContextConfig{TraceID: tid, SpanID: sid, TraceFlags: trace.FlagsSampled}))

	h := RequestInfo(nil)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		logger.FromContext(r.Context()).Warn("before auth")
		reqinfo.From(r.Context()).SetActor(&auth.Actor{Kind: auth.ActorToken, UserID: "u-7", TokenID: "t-1"})
		logger.FromContext(r.Context()).Error("after auth")
	}))
	req := httptest.NewRequest(http.MethodGet, "/api/x", nil).WithContext(spanCtx)
	req.Header.Set(RequestIDHeader, "req-1")
	h.ServeHTTP(httptest.NewRecorder(), req)

	lines := strings.Split(strings.TrimSpace(logs.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("want 2 lines, got:\n%s", logs.String())
	}
	traceAttr := "trace_id=4bf92f3577b34da6a3ce929d0e0e4736"
	if !strings.Contains(lines[0], "request_id=req-1") || !strings.Contains(lines[0], traceAttr) || strings.Contains(lines[0], "user_id") {
		t.Errorf("before the actor is recorded: want request_id and trace_id, no user_id:\n%s", lines[0])
	}
	if !strings.Contains(lines[1], "request_id=req-1") || !strings.Contains(lines[1], "user_id=u-7") || !strings.Contains(lines[1], traceAttr) {
		t.Errorf("after the actor is recorded: want request_id, user_id and trace_id:\n%s", lines[1])
	}

	// Outside a request: only an active span's trace id, and nothing at all without one.
	if got := LogAttrs(spanCtx); len(got) != 1 || got[0].Key != "trace_id" {
		t.Errorf("LogAttrs(span only) = %v", got)
	}
	if got := LogAttrs(context.Background()); len(got) != 0 {
		t.Errorf("LogAttrs(background) = %v, want none", got)
	}
	if logger.FromContext(context.Background()) != slog.Default() {
		t.Error("a context with nothing to add must log through slog.Default() itself")
	}
}

func TestRequestIDFromContext_PrefersTheInfo(t *testing.T) {
	ctx := context.WithValue(context.Background(), requestIDCtxKey{}, "legacy")
	if got := RequestIDFromContext(ctx); got != "legacy" {
		t.Errorf("without an Info: %q, want the id AccessLog stamped", got)
	}
	if got := RequestIDFromContext(reqinfo.With(ctx, &reqinfo.Info{ID: "info"})); got != "info" {
		t.Errorf("with an Info: %q, want the Info's id", got)
	}
}
