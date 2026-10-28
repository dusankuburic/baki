package ai

// Step 6 of the AI roadmap: the provider transports and the breaker.
//   - R-provider-4: every provider transport bounds the TLS handshake and the dial.
//   - R-provider-3: an upstream that accepts the connection and never answers is an outage (ErrProviderDown):
//     retried, counted by the breaker; a caller's own deadline during a half-open probe restarts no cooldown.
//   - R-provider-5: a non-streaming OpenAI-family request may wait for headers far longer than a streaming one.
//   - R-provider-6: a locally synthesised keepalive never resolves a half-open probe, and the exclusive probe
//     window is bounded.

import (
	"context"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/egress"
)

func transportOf(t *testing.T, c *http.Client) *http.Transport {
	t.Helper()
	tr, ok := c.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("transport is %T, want *http.Transport", c.Transport)
	}
	return tr
}

// R-provider-4, done when: the base transport and every client derived from it bound the TLS handshake, and the
// base one dials with a timeout.
func TestProviderTransports_BoundHandshakeAndDial(t *testing.T) {
	base := sharedTransportBase()
	if base.TLSHandshakeTimeout <= 0 {
		t.Fatalf("shared transport TLSHandshakeTimeout = %v, want > 0", base.TLSHandshakeTimeout)
	}
	if base.DialContext == nil {
		t.Error("shared transport has no DialContext: Go dials through a zero net.Dialer with no timeout")
	}
	ConfigureFixedHostClient(egress.AllowLoopback)
	t.Cleanup(func() {
		fixedHostRuntime.mu.Lock()
		fixedHostRuntime.client, fixedHostRuntime.longWait = sharedHTTPClient, longWaitClient(sharedTransportBase(), nil)
		fixedHostRuntime.mu.Unlock()
	})
	for name, c := range map[string]*http.Client{
		"fixed host":            fixedHostClient(),
		"fixed host, long wait": fixedHostClientFor(false),
		"azure (unconfigured)":  unconfiguredAzureClient(),
		"local models":          localModelsClient(),
		"auth":                  authHTTPClient,
	} {
		tr := transportOf(t, c)
		if tr.TLSHandshakeTimeout != base.TLSHandshakeTimeout {
			t.Errorf("%s: TLSHandshakeTimeout = %v, want the shared %v", name, tr.TLSHandshakeTimeout, base.TLSHandshakeTimeout)
		}
		if tr.DialContext == nil {
			t.Errorf("%s: no DialContext", name)
		}
	}
}

// A listener that accepts TCP and never speaks TLS fails the request at the handshake bound, not at the
// caller's deadline.
func TestProviderTransports_StalledHandshakeFailsAtTheBound(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			defer c.Close() // hold the connection open, say nothing
		}
	}()
	tr := sharedTransportBase().Clone()
	tr.TLSHandshakeTimeout = 200 * time.Millisecond // the shared value, scaled for the test
	client := &http.Client{Transport: tr}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, "https://"+ln.Addr().String()+"/", nil)
	start := time.Now()
	_, err = client.Do(req)
	if err == nil {
		t.Fatal("request succeeded against a silent TLS endpoint")
	}
	if took := time.Since(start); took > 5*time.Second {
		t.Errorf("request failed after %v, want about the handshake bound", took)
	}
}

// slowHeaderServer answers after delay, with an OpenAI-shaped reply; hits counts the requests.
func slowHeaderServer(t *testing.T, delay time.Duration, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select {
		case <-time.After(delay):
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"late"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func headerTimeoutClient(d time.Duration) *http.Client {
	tr := sharedTransportBase().Clone()
	tr.ResponseHeaderTimeout = d
	return &http.Client{Transport: tr}
}

// R-provider-3, done when: a header timeout is ErrProviderDown and the retry wrapper tries again.
func TestHeaderTimeout_IsProviderDownAndRetried(t *testing.T) {
	var hits atomic.Int32
	srv := slowHeaderServer(t, 2*time.Second, &hits)
	p := compatErrProvider(headerTimeoutClient(50*time.Millisecond), srv.URL+"/chat/completions")

	_, err := p.Chat(context.Background(), compatChatReq())
	if !errors.Is(err, ErrProviderDown) {
		t.Fatalf("Chat err = %v, want ErrProviderDown", err)
	}
	if !strings.Contains(err.Error(), "did not answer") {
		t.Errorf("err = %q, want it to say the provider did not answer", err)
	}

	hits.Store(0)
	rp := NewRetryingProviderWithConfig(p, 2, time.Millisecond)
	if _, err := rp.Chat(context.Background(), compatChatReq()); !errors.Is(err, ErrProviderDown) {
		t.Fatalf("retried Chat err = %v, want ErrProviderDown", err)
	}
	if n := hits.Load(); n != 2 {
		t.Errorf("attempts = %d, want 2 (a header timeout is retried)", n)
	}
}

// The caller's own deadline is not reclassified: it stays the caller's.
func TestHeaderTimeout_CallerDeadlineStaysTheCallers(t *testing.T) {
	var hits atomic.Int32
	srv := slowHeaderServer(t, 2*time.Second, &hits)
	p := compatErrProvider(headerTimeoutClient(time.Minute), srv.URL+"/chat/completions")
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	_, err := p.Chat(ctx, compatChatReq())
	if errors.Is(err, ErrProviderDown) {
		t.Errorf("a caller deadline read as ErrProviderDown: %v", err)
	}
}

// R-provider-3, done when: consecutive header timeouts on one provider|model open its circuit.
func TestHeaderTimeout_OpensTheCircuit(t *testing.T) {
	resetBreakerRegistry()
	t.Cleanup(resetBreakerRegistry)
	var hits atomic.Int32
	srv := slowHeaderServer(t, 2*time.Second, &hits)
	cb := NewCircuitBreakerProvider(compatErrProvider(headerTimeoutClient(30*time.Millisecond), srv.URL+"/chat/completions"))
	for range cbFailureThreshold {
		if _, err := cb.Chat(context.Background(), compatChatReq()); !errors.Is(err, ErrProviderDown) {
			t.Fatalf("want ErrProviderDown per call, got %v", err)
		}
	}
	if _, err := cb.Chat(context.Background(), compatChatReq()); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("want ErrCircuitOpen after %d header timeouts, got %v", cbFailureThreshold, err)
	}
}

// deadlineStub's Chat waits for the caller's context, or answers err at once when wait is false.
type deadlineStub struct {
	Provider
	wait bool
	err  error
}

func (s *deadlineStub) ID() string { return "deadline-stub" }
func (s *deadlineStub) Chat(ctx context.Context, _ Request) (*Response, error) {
	if s.wait {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if s.err != nil {
		return nil, s.err
	}
	return &Response{Content: "ok"}, nil
}

func openCircuitForProbe(t *testing.T, cb *CircuitBreakerProvider) {
	t.Helper()
	st := cb.stateFor("")
	st.mu.Lock()
	st.state = circuitOpen
	st.failures = cbFailureThreshold
	st.lastFailure = time.Now().Add(-cbOpenDuration - time.Millisecond)
	st.mu.Unlock()
}

// R-provider-3, done when: a half-open probe run under a 1 ms caller deadline restarts no cooldown (the next
// caller is the new probe); a probe that hits the upstream's header timeout does.
func TestCircuitBreaker_CallerDeadlineProbeRestartsNoCooldown(t *testing.T) {
	resetBreakerRegistry()
	t.Cleanup(resetBreakerRegistry)
	stub := &deadlineStub{wait: true}
	cb := NewCircuitBreakerProvider(stub)
	openCircuitForProbe(t, cb)

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()
	if _, err := cb.Chat(ctx, Request{}); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("probe err = %v, want the caller's deadline", err)
	}
	stub.wait = false
	if _, err := cb.Chat(context.Background(), Request{}); err != nil {
		t.Fatalf("the next caller was refused (%v): the caller's deadline restarted the cooldown", err)
	}

	openCircuitForProbe(t, cb)
	stub.err = errorsJoinDown("upstream did not answer within the header timeout", context.DeadlineExceeded)
	if _, err := cb.Chat(context.Background(), Request{}); !errors.Is(err, ErrProviderDown) {
		t.Fatalf("probe err = %v, want ErrProviderDown", err)
	}
	stub.err = nil
	if _, err := cb.Chat(context.Background(), Request{}); !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("after an upstream header timeout the cooldown must restart, got %v", err)
	}
}

func errorsJoinDown(msg string, cause error) error {
	return &wrappedDown{msg: msg, cause: cause}
}

type wrappedDown struct {
	msg   string
	cause error
}

func (w *wrappedDown) Error() string   { return w.msg }
func (w *wrappedDown) Unwrap() []error { return []error{ErrProviderDown, w.cause} }

// R-provider-5, done when: a stream-refusing OpenAI-family deployment whose non-streaming answer takes longer than
// the streaming header floor still completes, and a streaming request whose headers never come fails at the short
// floor.
func TestFixedHost_NonStreamingRequestsWaitLonger(t *testing.T) {
	restore := setFixedHostHeaderTimeouts(t, 100*time.Millisecond, 5*time.Second)
	defer restore()

	var hits atomic.Int32
	srv := slowHeaderServer(t, 300*time.Millisecond, &hits)
	p := &OpenAIProvider{openaiBase: openaiBase{apiKey: "sk-test", client: fixedHostClient(), longWaitClient: fixedHostClientFor(false),
		baseURL: ptr(srv.URL + "/chat/completions"), providerLabel: "openai"}}

	resp, err := p.Chat(context.Background(), compatChatReq())
	if err != nil || resp.Content != "late" {
		t.Fatalf("non-streaming Chat = %+v, %v; want the late answer (the long header wait)", resp, err)
	}

	err = p.Stream(context.Background(), Request{Model: "gpt-4o", Messages: []Message{{Role: "user", Content: "hi"}}}, func(Chunk) {})
	if !errors.Is(err, ErrProviderDown) {
		t.Errorf("streaming request err = %v, want ErrProviderDown at the short floor", err)
	}
}

func ptr[T any](v T) *T { return &v }

// setFixedHostHeaderTimeouts installs fixed-host clients with the given header floors (unguarded clones of the
// shared transport) and returns the restore.
func setFixedHostHeaderTimeouts(t *testing.T, streaming, longWait time.Duration) func() {
	t.Helper()
	fixedHostRuntime.mu.Lock()
	prevClient, prevLong := fixedHostRuntime.client, fixedHostRuntime.longWait
	short := sharedTransportBase().Clone()
	short.ResponseHeaderTimeout = streaming
	long := sharedTransportBase().Clone()
	long.ResponseHeaderTimeout = longWait
	fixedHostRuntime.client = &http.Client{Transport: short}
	fixedHostRuntime.longWait = &http.Client{Transport: long}
	fixedHostRuntime.mu.Unlock()
	return func() {
		fixedHostRuntime.mu.Lock()
		fixedHostRuntime.client, fixedHostRuntime.longWait = prevClient, prevLong
		fixedHostRuntime.mu.Unlock()
	}
}

// R-provider-6 (a), done when: a half-open probe whose stream emits only a local keepalive and then an outage
// reopens the circuit; no other caller was admitted in between.
func TestCircuitBreaker_LocalKeepaliveDoesNotResolveTheProbe(t *testing.T) {
	resetBreakerRegistry()
	t.Cleanup(resetBreakerRegistry)
	var admittedDuring error
	var cb *CircuitBreakerProvider
	stub := &callbackStreamStub{run: func(onChunk func(Chunk)) {
		onChunk(Chunk{LocalKeepalive: true})
		_, admittedDuring = cb.Chat(context.Background(), Request{})
		onChunk(Chunk{Error: ErrProviderDown})
	}}
	cb = NewCircuitBreakerProvider(stub)
	openCircuitForProbe(t, cb)
	_ = cb.Stream(context.Background(), Request{}, func(Chunk) {})
	if !errors.Is(admittedDuring, ErrCircuitOpen) {
		t.Errorf("a caller during the probe got %v, want ErrCircuitOpen (the keepalive resolved the probe)", admittedDuring)
	}
	st := cb.stateFor("")
	st.mu.Lock()
	state, last := st.state, st.lastFailure
	st.mu.Unlock()
	if state != circuitOpen || time.Since(last) > time.Second {
		t.Errorf("after the probe: state %v, last failure %v ago; want open with the cooldown restarted", state, time.Since(last))
	}

	// An upstream keepalive (an empty chunk the provider sent) still resolves it.
	openCircuitForProbe(t, cb)
	stub.run = func(onChunk func(Chunk)) {
		onChunk(Chunk{})
		_, admittedDuring = cb.Chat(context.Background(), Request{})
		onChunk(Chunk{Text: "hi"})
	}
	_ = cb.Stream(context.Background(), Request{}, func(Chunk) {})
	if admittedDuring != nil {
		t.Errorf("after an upstream keepalive the circuit must be closed, a caller got %v", admittedDuring)
	}
}

// R-provider-6 (b), done when: a caller arriving after the bounded probe window is admitted while the probe is
// still in flight.
func TestCircuitBreaker_ProbeWindowIsBounded(t *testing.T) {
	resetBreakerRegistry()
	t.Cleanup(resetBreakerRegistry)
	cb := NewCircuitBreakerProvider(&deadlineStub{})
	openCircuitForProbe(t, cb)
	st := cb.stateFor("")
	if err := cb.checkSt(context.Background(), st); err != nil {
		t.Fatalf("the probe was refused: %v", err)
	}
	if err := cb.checkSt(context.Background(), st); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("a second caller inside the window got %v, want ErrCircuitOpen", err)
	}
	st.mu.Lock()
	st.probeStarted = time.Now().Add(-cbProbeWindow - time.Millisecond)
	st.mu.Unlock()
	if err := cb.checkSt(context.Background(), st); err != nil {
		t.Errorf("a caller after the probe window got %v, want admitted", err)
	}
}

// callbackStreamStub runs its stream body with the caller's callback.
type callbackStreamStub struct {
	Provider
	run func(onChunk func(Chunk))
}

func (s *callbackStreamStub) ID() string { return "callback-stream" }
func (s *callbackStreamStub) Chat(context.Context, Request) (*Response, error) {
	return &Response{Content: "ok"}, nil
}
func (s *callbackStreamStub) Stream(_ context.Context, _ Request, onChunk func(Chunk)) error {
	s.run(onChunk)
	return nil
}

// Review: past the probe window one more caller is admitted, not everyone.
func TestCircuitBreaker_ProbeWindowAdmitsOneMore(t *testing.T) {
	resetBreakerRegistry()
	t.Cleanup(resetBreakerRegistry)
	cb := NewCircuitBreakerProvider(&deadlineStub{})
	openCircuitForProbe(t, cb)
	st := cb.stateFor("")
	_ = cb.checkSt(context.Background(), st)
	st.mu.Lock()
	st.probeStarted = time.Now().Add(-cbProbeWindow - time.Millisecond)
	st.mu.Unlock()
	if err := cb.checkSt(context.Background(), st); err != nil {
		t.Fatalf("the extra probe was refused: %v", err)
	}
	if err := cb.checkSt(context.Background(), st); !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("a caller right after the extra probe got %v, want ErrCircuitOpen", err)
	}
}

// Review: a timeout on a long-wait client is an outage the breaker counts but the retry wrapper never replays
// (the upstream may have produced and billed the answer).
func TestLongWaitTimeout_IsNotReplayed(t *testing.T) {
	long := longWaitClient(sharedTransportBase(), nil)
	if !waitsLong(long) || waitsLong(fixedHostClient()) {
		t.Fatal("waitsLong must tell the long-wait client from the streaming one")
	}
	timeout := &upstreamTimeoutError{cause: errors.New("net/http: timeout awaiting response headers"), noReplay: true}
	if !errors.Is(timeout, ErrProviderDown) || !isRetryable(timeout) || !replayForbidden(timeout) {
		t.Fatal("a long-wait timeout is ErrProviderDown (counted) and not replayed")
	}
	stub := &retryStub{chatErrs: []error{timeout, nil}}
	if _, err := NewRetryingProviderWithConfig(stub, 3, time.Millisecond).Chat(context.Background(), Request{}); !errors.Is(err, ErrProviderDown) {
		t.Fatalf("err = %v, want the timeout", err)
	}
	if stub.chatCalls != 1 {
		t.Errorf("calls = %d, want 1 (no replay)", stub.chatCalls)
	}
}

// Review: every OpenAI-family constructor wires the long-wait client, and after boot it carries the egress guard
// and the redirect refusal like the streaming one.
func TestFixedHost_LongWaitClientWiredAndGuarded(t *testing.T) {
	ConfigureFixedHostClient(egress.BlockInternal)
	t.Cleanup(func() {
		fixedHostRuntime.mu.Lock()
		fixedHostRuntime.client, fixedHostRuntime.longWait = sharedHTTPClient, longWaitClient(sharedTransportBase(), nil)
		fixedHostRuntime.mu.Unlock()
	})
	long := fixedHostClientFor(false)
	for name, got := range map[string]*http.Client{
		"openai":        NewOpenAIProvider("k").longWaitClient,
		"xai":           NewXAIProvider("k").longWaitClient,
		"glm":           NewGLMProvider("k").longWaitClient,
		"github-models": NewGitHubModelsProvider("k").longWaitClient,
		"copilot":       NewCopilotProvider("k").longWaitClient,
	} {
		if got != long {
			t.Errorf("%s: longWaitClient is not the fixed-host long-wait client", name)
		}
	}
	if long.CheckRedirect == nil || !errors.Is(long.CheckRedirect(nil, nil), http.ErrUseLastResponse) {
		t.Error("the long-wait client follows redirects")
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodGet, srv.URL, nil)
	if resp, err := long.Do(req); err == nil {
		resp.Body.Close()
		t.Error("the long-wait client reached a loopback address under BlockInternal: no egress guard")
	}
}

// R-provider-5, done when: a stream-refusing deployment answered through Stream → streamViaChat waits the long
// header floor, not the streaming one.
func TestFixedHost_StreamViaChatWaitsLonger(t *testing.T) {
	restore := setFixedHostHeaderTimeouts(t, 100*time.Millisecond, 5*time.Second)
	defer restore()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body map[string]any
		_ = json.NewDecoder(r.Body).Decode(&body)
		w.Header().Set("Content-Type", "application/json")
		if body["stream"] == true {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = w.Write([]byte(`{"error":{"message":"Unsupported value: 'stream' does not support true with this model.","type":"invalid_request_error","param":"stream","code":"unsupported_value"}}`))
			return
		}
		time.Sleep(300 * time.Millisecond)
		_, _ = w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"late"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer srv.Close()
	p := &OpenAIProvider{openaiBase: openaiBase{apiKey: "sk-test", client: fixedHostClient(), longWaitClient: fixedHostClientFor(false),
		baseURL: ptr(srv.URL + "/chat/completions"), providerLabel: "openai"}}
	var text string
	err := p.Stream(context.Background(), Request{Model: "o1-refuses-stream-" + t.Name(), Messages: []Message{{Role: "user", Content: "hi"}}},
		func(c Chunk) { text += c.Text })
	if err != nil || text != "late" {
		t.Errorf("Stream via chat = %q, %v; want the late answer", text, err)
	}
}
