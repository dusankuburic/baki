package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"pad-core/logger"
)

// upstreamLogCtxKey marks the test's context so the installed extractor can prove the line was logged through
// logger.FromContext with the caller's ctx, not through the bare default logger.
type upstreamLogCtxKey struct{}

// captureUpstreamLog routes slog's default logger into a JSON buffer and installs a context extractor that
// reports the upstreamLogCtxKey value as request_id; both are restored when the test ends.
func captureUpstreamLog(t *testing.T) *bytes.Buffer {
	t.Helper()
	buf := &bytes.Buffer{}
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewJSONHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	logger.SetContextAttrs(func(ctx context.Context) []slog.Attr {
		if v, ok := ctx.Value(upstreamLogCtxKey{}).(string); ok {
			return []slog.Attr{slog.String("request_id", v)}
		}
		return nil
	})
	t.Cleanup(func() {
		slog.SetDefault(prev)
		logger.SetContextAttrs(nil)
	})
	return buf
}

// upstreamErrorLines returns the decoded "AI upstream error" records in buf.
func upstreamErrorLines(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		var rec map[string]any
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("undecodable log line %q: %v", line, err)
		}
		if rec["msg"] == "AI upstream error" {
			out = append(out, rec)
		}
	}
	return out
}

// R-observability-2: a 503 from a gateway that sends no request-id header was never logged — the helper
// returned before logging when there was no id to report — so an outage left no line at all. Every non-2xx
// upstream response is logged exactly once, with the caller's request context, and the request id only when
// the upstream sent one.
func TestUpstreamRejection_LoggedWithoutRequestID(t *testing.T) {
	buf := captureUpstreamLog(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	p := compatErrProvider(srv.Client(), srv.URL+"/chat/completions")

	ctx := context.WithValue(context.Background(), upstreamLogCtxKey{}, "req-under-test")
	if _, err := p.Chat(ctx, compatChatReq()); err == nil {
		t.Fatal("a 503 must fail the call")
	}

	lines := upstreamErrorLines(t, buf)
	if n := int(hits.Load()); len(lines) != n || n != 1 {
		t.Fatalf("want exactly one \"AI upstream error\" line for the one 503, got %d lines for %d responses\n%s",
			len(lines), n, buf.String())
	}
	rec := lines[0]
	if rec["level"] != "WARN" {
		t.Errorf("level = %v, want WARN", rec["level"])
	}
	if rec["status"] != float64(http.StatusServiceUnavailable) {
		t.Errorf("status = %v, want 503", rec["status"])
	}
	if rec["provider"] != "openai" || rec["model"] != "gpt-4o" {
		t.Errorf("provider/model = %v/%v, want openai/gpt-4o", rec["provider"], rec["model"])
	}
	if _, ok := rec["requestId"]; ok {
		t.Errorf("requestId must be omitted when the upstream sent none, got %v", rec["requestId"])
	}
	if rec["request_id"] != "req-under-test" {
		t.Errorf("request_id = %v, want the caller's context attribute (logged via logger.FromContext)", rec["request_id"])
	}
}

// The request id is still reported when the upstream sends one.
func TestUpstreamRejection_LoggedWithRequestID(t *testing.T) {
	buf := captureUpstreamLog(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("apim-request-id", "apim-123")
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	p := compatErrProvider(srv.Client(), srv.URL+"/chat/completions")

	if _, err := p.Chat(context.Background(), compatChatReq()); err == nil {
		t.Fatal("a 503 must fail the call")
	}
	lines := upstreamErrorLines(t, buf)
	if len(lines) != 1 {
		t.Fatalf("want one \"AI upstream error\" line, got %d\n%s", len(lines), buf.String())
	}
	if lines[0]["requestId"] != "apim-123" {
		t.Errorf("requestId = %v, want apim-123", lines[0]["requestId"])
	}
}

// The Claude provider has the same helper and the same rule: a 503 without request-id or apim-request-id is
// logged once, without empty id fields.
func TestClaudeUpstreamRejection_LoggedWithoutRequestID(t *testing.T) {
	buf := captureUpstreamLog(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	p := NewClaudeProvider("test-key")
	p.client = srv.Client()
	orig := claudeAPIURL
	t.Cleanup(func() { claudeAPIURL = orig })
	claudeAPIURL = srv.URL

	ctx := context.WithValue(context.Background(), upstreamLogCtxKey{}, "req-claude")
	if _, err := p.Chat(ctx, Request{Model: "claude-sonnet-4-5", Messages: []Message{{Role: "user", Content: "hi"}}}); err == nil {
		t.Fatal("a 503 must fail the call")
	}
	lines := upstreamErrorLines(t, buf)
	if n := int(hits.Load()); len(lines) != n || n != 1 {
		t.Fatalf("want exactly one \"AI upstream error\" line for the one 503, got %d lines for %d responses\n%s",
			len(lines), n, buf.String())
	}
	rec := lines[0]
	if rec["status"] != float64(http.StatusServiceUnavailable) || rec["provider"] == nil || rec["provider"] == "" {
		t.Errorf("status/provider = %v/%v, want 503 and a provider label", rec["status"], rec["provider"])
	}
	for _, k := range []string{"requestId", "apimRequestId"} {
		if _, ok := rec[k]; ok {
			t.Errorf("%s must be omitted when the upstream sent none, got %v", k, rec[k])
		}
	}
	if rec["request_id"] != "req-claude" {
		t.Errorf("request_id = %v, want the caller's context attribute (logged via logger.FromContext)", rec["request_id"])
	}
}

// Gemini's chat, stream and embedding calls log their non-2xx too (the Google API sends no request id header).
func TestGeminiUpstreamRejection_Logged(t *testing.T) {
	buf := captureUpstreamLog(t)
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
		_, _ = w.Write([]byte(`{"error":{"code":503,"message":"overloaded","status":"UNAVAILABLE"}}`))
	}))
	defer srv.Close()
	prev := geminiBaseHost
	geminiBaseHost = srv.URL
	defer func() { geminiBaseHost = prev }()
	p := &GeminiProvider{apiKey: "k", client: srv.Client()}

	ctx := context.WithValue(context.Background(), upstreamLogCtxKey{}, "req-gemini")
	req := Request{Model: "gemini-2.5-flash", Messages: []Message{{Role: "user", Content: "hi"}}}
	if _, err := p.Chat(ctx, req); err == nil {
		t.Fatal("a 503 must fail the chat call")
	}
	if err := p.Stream(ctx, req, func(Chunk) {}); err == nil {
		t.Fatal("a 503 must fail the stream call")
	}
	if _, err := p.Embed(ctx, []string{"x"}); err == nil {
		t.Fatal("a 503 must fail the embed call")
	}

	lines := upstreamErrorLines(t, buf)
	if n := int(hits.Load()); len(lines) != n || n < 3 {
		t.Fatalf("%d upstream error lines for %d responses, want one each", len(lines), n)
	}
	for _, l := range lines {
		if l["provider"] != "gemini" || l["status"] != float64(503) || l["request_id"] != "req-gemini" || l["level"] != "WARN" {
			t.Errorf("line %v, want provider gemini, status 503, the caller's request id, at WARN", l)
		}
	}
}
