package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// TestAzureFoundry_Repairs422ExtraParameters (D-27): the Azure AI Model
// Inference layer's HTTP 422 "Extra parameters ['x'] are not allowed …" is a
// parameter rejection: max_completion_tokens is swapped to max_tokens on one
// repair round trip instead of failing every turn.
func TestAzureFoundry_Repairs422ExtraParameters(t *testing.T) {
	resetLearnedRepairs()
	t.Cleanup(resetLearnedRepairs)
	srv := newAzureTestServer(t, func(w http.ResponseWriter, _ *http.Request, _ int, body map[string]any) {
		if _, has := body["max_completion_tokens"]; has {
			writeJSON(w, http.StatusUnprocessableEntity, map[string]any{"error": map[string]any{
				"code":    "UnprocessableEntity",
				"message": "Extra parameters ['max_completion_tokens'] are not allowed when extra-parameters is not set or set to be 'error'.",
			}})
			return
		}
		writeJSON(w, 200, chatCompletionJSON("ok", "Phi-4"))
	})
	p := newAzureKeyProvider(t, srv, nil)
	resp, err := p.Chat(context.Background(), Request{Model: "phi", MaxTokens: 64, Messages: []Message{{Role: "user", Content: "hi"}}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("content = %q", resp.Content)
	}
	reqs := srv.requests()
	if len(reqs) != 2 {
		t.Fatalf("%d requests, want 2 (one repair round trip)", len(reqs))
	}
	if _, has := reqs[1].Body["max_completion_tokens"]; has || reqs[1].Body["max_tokens"] != float64(64) {
		t.Errorf("second request = %v, want max_tokens 64 and no max_completion_tokens", reqs[1].Body)
	}
}

// TestUnsupportedParam_ParameterRejectionStatuses (D-27): the 422 wording is
// recognised for any repairable parameter; other statuses are not parameter
// rejections.
func TestUnsupportedParam_ParameterRejectionStatuses(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
		ok     bool
	}{
		{"422 extra parameters", 422, `{"error":{"code":"UnprocessableEntity","message":"Extra parameters ['max_completion_tokens'] are not allowed when extra-parameters is not set or set to be 'error'."}}`, "max_completion_tokens", true},
		{"422 stream_options", 422, `{"error":{"message":"Extra parameters ['stream_options'] are not allowed"}}`, "stream_options", true},
		{"422 several: first wins", 422, `{"error":{"message":"Extra parameters ['temperature', 'top_p'] are not allowed"}}`, "temperature", true},
		{"400 structured", 400, `{"error":{"code":"unsupported_value","param":"temperature","message":"Unsupported value"}}`, "temperature", true},
		{"400 extra parameter", 400, `{"error":{"message":"Extra parameter 'seed' is not allowed"}}`, "seed", true},
		{"500 is not a rejection", 500, `{"error":{"message":"Extra parameters ['temperature'] are not allowed"}}`, "", false},
		{"422 unrelated", 422, `{"error":{"message":"input is invalid"}}`, "", false},
	} {
		got, ok := unsupportedParam(tc.status, []byte(tc.body))
		if got != tc.want || ok != tc.ok {
			t.Errorf("%s: unsupportedParam = %q %v, want %q %v", tc.name, got, ok, tc.want, tc.ok)
		}
	}
	if !parameterRejectionStatus(400) || !parameterRejectionStatus(422) || parameterRejectionStatus(404) || parameterRejectionStatus(500) {
		t.Error("parameterRejectionStatus must accept exactly 400 and 422")
	}
	if reasoningToolsRejected(422, []byte(`{"error":{"message":"Function tools with reasoning_effort are not supported for this model"}}`)) != true {
		t.Error("the tools-with-reasoning rejection is recognised on 422 too")
	}
}

// TestSend_ResidualErrorCarriesUpstreamRequestID (D-29): the gateway's
// correlation id (apim-request-id, x-ms-request-id, x-request-id) is read
// from every non-200 and appended to the unclassified error texts; the
// sentinel-wrapped classifications are untouched.
func TestSend_ResidualErrorCarriesUpstreamRequestID(t *testing.T) {
	var status atomic.Int32
	status.Store(http.StatusTeapot)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("apim-request-id", "abc-123")
		w.WriteHeader(int(status.Load()))
	}))
	defer srv.Close()
	p := compatErrProvider(srv.Client(), srv.URL+"/chat/completions")

	if _, err := p.Chat(context.Background(), compatChatReq()); err == nil || !strings.Contains(err.Error(), "request id abc-123") {
		t.Errorf("chat residual error = %v, want the request id", err)
	}
	if err := p.Stream(context.Background(), compatChatReq(), func(Chunk) {}); err == nil || !strings.Contains(err.Error(), "request id abc-123") {
		t.Errorf("stream residual error = %v, want the request id", err)
	}
	status.Store(http.StatusServiceUnavailable)
	if _, err := p.Chat(context.Background(), compatChatReq()); !errors.Is(err, ErrProviderDown) {
		t.Errorf("a 503 with a request id is still ErrProviderDown, got %v", err)
	}

	mk := func(h, v string) *http.Response {
		r := &http.Response{Header: http.Header{}}
		r.Header.Set(h, v)
		return r
	}
	if upstreamRequestID(nil) != "" || upstreamRequestID(&http.Response{Header: http.Header{}}) != "" {
		t.Error("no header, no id")
	}
	both := mk("x-request-id", "openai")
	both.Header.Set("x-ms-request-id", "ms")
	both.Header.Set("apim-request-id", "apim")
	if got := upstreamRequestID(both); got != "apim" {
		t.Errorf("apim-request-id wins, got %q", got)
	}
	if got := upstreamRequestID(mk("X-Request-Id", "oa")); got != "oa" {
		t.Errorf("x-request-id read case-insensitively, got %q", got)
	}
}

// repairTestServer answers chat completions; reject decides per request body
// whether (and how) to reject it. Bodies are recorded raw.
type repairTestServer struct {
	*httptest.Server
	mu     sync.Mutex
	bodies [][]byte
	reject func(body map[string]any) (status int, resp map[string]any)
}

func newRepairTestServer(t *testing.T) *repairTestServer {
	t.Helper()
	s := &repairTestServer{}
	s.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		s.mu.Lock()
		s.bodies = append(s.bodies, raw)
		reject := s.reject
		s.mu.Unlock()
		if reject != nil {
			if status, resp := reject(body); status != 0 {
				writeJSON(w, status, resp)
				return
			}
		}
		if body["stream"] == true {
			w.Header().Set("Content-Type", "text/event-stream")
			_, _ = w.Write([]byte(`data: {"model":"gpt-5","choices":[{"delta":{"content":"ok"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"))
			return
		}
		writeJSON(w, 200, chatCompletionJSON("ok", "gpt-5"))
	}))
	t.Cleanup(s.Close)
	return s
}

func (s *repairTestServer) setReject(f func(body map[string]any) (int, map[string]any)) {
	s.mu.Lock()
	s.reject = f
	s.mu.Unlock()
}

func (s *repairTestServer) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bodies)
}

func (s *repairTestServer) body(i int) map[string]any {
	s.mu.Lock()
	defer s.mu.Unlock()
	var m map[string]any
	_ = json.Unmarshal(s.bodies[i], &m)
	return m
}

func rejectParam(param string) (int, map[string]any) {
	return 400, map[string]any{"error": map[string]any{
		"code": "unsupported_parameter", "param": param,
		"message": "Unsupported parameter: '" + param + "' is not supported with this model.",
	}}
}

// TestSend_RemembersRepairsAcrossRequests (PERF-6): a repair a 200 confirmed
// is pre-applied to later requests for the same endpoint + model — across
// provider instances — so the rejection round trip is paid once, not on
// every turn; another model on the endpoint learns its own.
func TestSend_RemembersRepairsAcrossRequests(t *testing.T) {
	resetLearnedRepairs()
	t.Cleanup(resetLearnedRepairs)
	srv := newRepairTestServer(t)
	srv.setReject(func(body map[string]any) (int, map[string]any) {
		if _, has := body["temperature"]; has {
			return rejectParam("temperature")
		}
		return 0, nil
	})
	req := Request{Model: "o4-mini", Temperature: 0.3, Messages: []Message{{Role: "user", Content: "hi"}}}
	p := compatErrProvider(srv.Client(), srv.URL+"/chat/completions")

	for i := range 3 {
		if _, err := p.Chat(context.Background(), req); err != nil {
			t.Fatalf("chat %d: %v", i, err)
		}
	}
	if n := srv.count(); n != 4 {
		t.Errorf("3 turns cost %d requests, want 4 (one rejection, then repaired up front)", n)
	}
	if _, has := srv.body(2)["temperature"]; has {
		t.Error("the second turn still carried temperature")
	}

	// A fresh instance (the factory builds one per request) and the stream
	// path share the memo.
	q := compatErrProvider(srv.Client(), srv.URL+"/chat/completions")
	if err := q.Stream(context.Background(), req, func(Chunk) {}); err != nil {
		t.Fatalf("stream: %v", err)
	}
	if n := srv.count(); n != 5 {
		t.Errorf("a fresh instance paid %d extra requests, want 1", n-4)
	}

	// Another model on the same endpoint relearns.
	other := req
	other.Model = "o3"
	if _, err := q.Chat(context.Background(), other); err != nil {
		t.Fatal(err)
	}
	if n := srv.count(); n != 7 {
		t.Errorf("another model cost %d requests, want 2", n-5)
	}
	if got := repairsFor(q.repairsKey("o4-mini")); len(got) != 1 || got[0] != "temperature" {
		t.Errorf("memo = %v, want [temperature]", got)
	}
}

// TestSend_ForgetsARejectedLearnedRepair (PERF-6): when the model no longer
// agrees with a remembered repair (the deployment was re-pointed), the memo is
// forgotten and the parameter repaired afresh — the turn does not fail, and
// the next request starts from the raw payload.
func TestSend_ForgetsARejectedLearnedRepair(t *testing.T) {
	resetLearnedRepairs()
	t.Cleanup(resetLearnedRepairs)
	srv := newRepairTestServer(t)
	// Phase 1: a gpt-5.6-style deployment refuses tools until reasoning is off.
	srv.setReject(func(body map[string]any) (int, map[string]any) {
		if body["reasoning_effort"] != "none" {
			return 400, map[string]any{"error": map[string]any{"message": "Function tools with reasoning_effort are not supported for gpt-5.6 in /v1/chat/completions. To use function tools, use /v1/responses or set reasoning_effort to 'none'."}}
		}
		return 0, nil
	})
	req := Request{Model: "chat", Messages: []Message{{Role: "user", Content: "hi"}},
		Tools: []ToolDefinition{{Name: "search_flow", InputSchema: []byte(`{"type":"object"}`)}}}
	p := compatErrProvider(srv.Client(), srv.URL+"/chat/completions")
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatalf("phase 1: %v", err)
	}
	if n := srv.count(); n != 2 {
		t.Fatalf("phase 1 cost %d requests, want 2", n)
	}
	key := p.repairsKey("chat")
	if got := repairsFor(key); len(got) != 1 || got[0] != reasoningEffortNoneRepair {
		t.Fatalf("memo = %v, want [%s]", got, reasoningEffortNoneRepair)
	}

	// Phase 2: the deployment now points at a model that rejects
	// reasoning_effort outright.
	srv.setReject(func(body map[string]any) (int, map[string]any) {
		if _, has := body["reasoning_effort"]; has {
			return rejectParam(reasoningEffortParam)
		}
		return 0, nil
	})
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatalf("phase 2: the turn must not fail on a stale memo: %v", err)
	}
	if n := srv.count(); n != 4 {
		t.Errorf("phase 2 cost %d requests, want 2 (the stale rewrite, then repaired)", n-2)
	}
	if _, has := srv.body(3)[reasoningEffortParam]; has {
		t.Error("the rejected pre-applied parameter was not removed")
	}
	if got := repairsFor(key); got != nil {
		t.Errorf("a rejected memo must be forgotten, got %v", got)
	}
	// Phase 3: the next request starts raw and succeeds at once.
	if _, err := p.Chat(context.Background(), req); err != nil {
		t.Fatal(err)
	}
	if n := srv.count(); n != 5 {
		t.Errorf("phase 3 cost %d requests, want 1", n-4)
	}
}

// TestSend_EncodesTheRequestOnce (PERF-15): with nothing to repair the wire
// body is the struct's own encoding (field order), not a re-encoded generic
// map (sorted keys) — the encode→decode→encode round is gone.
func TestSend_EncodesTheRequestOnce(t *testing.T) {
	resetLearnedRepairs()
	t.Cleanup(resetLearnedRepairs)
	srv := newRepairTestServer(t)
	p := compatErrProvider(srv.Client(), srv.URL+"/chat/completions")
	if _, err := p.Chat(context.Background(), Request{Model: "gpt-4o", Temperature: 0.3, MaxTokens: 5, Messages: []Message{{Role: "user", Content: "hi"}}}); err != nil {
		t.Fatal(err)
	}
	srv.mu.Lock()
	raw := string(srv.bodies[0])
	srv.mu.Unlock()
	if !strings.HasPrefix(raw, `{"model":"gpt-4o"`) {
		t.Errorf("body was re-encoded from a map (sorted keys): %s", raw)
	}
}

// TestStream_DrainsBodyForConnectionReuse (PERF-13): after [DONE] the rest
// of the body is read before Close, so HTTP/1.1 connections return to the
// pool instead of being discarded (a new TCP + TLS handshake per turn).
func TestStream_DrainsBodyForConnectionReuse(t *testing.T) {
	var conns atomic.Int32
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(`data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}` + "\n\ndata: [DONE]\n\n"))
	}))
	srv.Config.ConnState = func(_ net.Conn, s http.ConnState) {
		if s == http.StateNew {
			conns.Add(1)
		}
	}
	srv.Start()
	defer srv.Close()
	p := compatErrProvider(srv.Client(), srv.URL+"/chat/completions")

	const streams = 8
	for i := range streams {
		if err := p.Stream(context.Background(), compatChatReq(), func(Chunk) {}); err != nil {
			t.Fatalf("stream %d: %v", i, err)
		}
	}
	if n := conns.Load(); n > 3 {
		t.Errorf("%d streams opened %d connections; the drained body should let the pool reuse them", streams, n)
	}
}
