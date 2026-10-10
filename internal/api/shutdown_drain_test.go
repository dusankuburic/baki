package api

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/api/render"
	"pad-analyzer/internal/config"
	"pad-analyzer/internal/platform/drain"
	"pad-analyzer/internal/service"
	"pad-analyzer/internal/storage"
	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// The server's stop as clients see it (ADR 0024): while draining, the readiness probe is 503 at once and new chat
// streams, event streams and sockets are refused with 503 SERVER_RESTARTING and a Retry-After; a running chat stream
// gets its terminal SERVER_RESTARTING event over its still-open SSE connection before the connection closes.

// checkServerRestarting asserts the drain refusal: 503, Retry-After 2, and the standard envelope with code
// SERVER_RESTARTING and details.retryAfterSeconds 2 (the web client reads the body, not the header).
func checkServerRestarting(t *testing.T, rr *httptest.ResponseRecorder) {
	t.Helper()
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("status %d, want 503 (body %s)", rr.Code, rr.Body.String())
	}
	if got := rr.Header().Get("Retry-After"); got != "2" {
		t.Errorf("Retry-After %q, want 2", got)
	}
	var env render.ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil || env.Code != "SERVER_RESTARTING" || env.Details["retryAfterSeconds"] != float64(2) {
		t.Errorf("body %s (%v), want code SERVER_RESTARTING and details.retryAfterSeconds 2", rr.Body.String(), err)
	}
}

// While draining, a new chat stream is refused before the body is read: nothing starts, so the client's one retry
// after Retry-After is safe.
func TestChat_DrainingRefusesNewStreams(t *testing.T) {
	rt := newTestRouter(nil, false)
	st := drain.New()
	moduleOf[*ChatHandler](t, rt).draining = st
	st.Begin()

	rr := doRequest(t, rt, http.MethodPost, "/api/chat/stream", map[string]any{"provider": "fake", "userMessage": "hi"})
	checkServerRestarting(t, rr)
}

// Once the chat service's own shutdown has begun (the stop hook calls it after the drain pause), a stream that got
// past the handler's check is refused the same way.
func TestChat_ShutdownServiceRefusesNewStreams(t *testing.T) {
	store := testutil.NewFakeBackend()
	rt := newTestRouter(store, true)
	if err := rt.security.Backend.SaveFlow(context.Background(), &interfaces.FlowDocument{ID: "f-drain", Name: "t", OwnerID: "u1"}); err != nil {
		t.Fatalf("seed flow: %v", err)
	}
	bearer := jwtBearer(t, rt, "u1", "u1@example.com")
	moduleOf[*ChatHandler](t, rt).draining = drain.New()
	if err := moduleOf[*ChatHandler](t, rt).chatSvc.Shutdown(context.Background()); err != nil {
		t.Fatalf("Shutdown: %v", err)
	}

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/stream", bearer, map[string]any{
		"flowId": "f-drain", "provider": "claude", "userMessage": "hi", "clientStreamId": uuid.NewString(),
	})
	checkServerRestarting(t, rr)
}

// The readiness probe takes a draining replica out of rotation on the first probe, without the three-failure
// smoothing a dependency blip gets; liveness stays 200 (a draining process is not dead).
func TestReadyz_DrainingIs503Immediately(t *testing.T) {
	rt := newTestRouter(testutil.NewFakeBackend(), false)
	st := drain.New()
	moduleOf[*SystemHandler](t, rt).draining = st

	if rr := doRequestWithAuth(t, rt, http.MethodGet, "/readyz", "", nil); rr.Code != http.StatusOK {
		t.Fatalf("/readyz before draining: %d", rr.Code)
	}
	st.Begin()
	rr := doRequestWithAuth(t, rt, http.MethodGet, "/readyz", "", nil)
	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("/readyz while draining: %d, want 503 on the first probe", rr.Code)
	}
	var body map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &body); err != nil || body["status"] != "draining" {
		t.Errorf("/readyz body %s, want {\"status\":\"draining\"}", rr.Body.String())
	}
	if rr := doRequestWithAuth(t, rt, http.MethodGet, "/healthz", "", nil); rr.Code != http.StatusOK {
		t.Errorf("/healthz while draining: %d, want 200", rr.Code)
	}
}

// While draining, a new event stream is refused (the client's reconnect goes to another replica). One already open
// stays open until the stop closes SSE: TestShutdown_ChatStreamGetsServerRestartingBeforeSSECloses.
func TestEvents_DrainingRefusesNewConnections(t *testing.T) {
	m := NewEventManager(make(chan struct{}))
	st := drain.New()
	m.draining = st
	st.Begin()

	// Bounded: an event stream that is not refused would otherwise run until the request ends.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	rr := httptest.NewRecorder()
	m.HandleEvents(rr, httptest.NewRequest(http.MethodGet, "/api/events", nil).WithContext(ctx))
	checkServerRestarting(t, rr)
	if m.HasSubscriber("") {
		t.Error("a refused event stream was registered")
	}
}

// gatedWriter is an SSE response writer whose event writes wait for gate, so frames pile up in the client's queue.
type gatedWriter struct {
	header  http.Header
	gate    chan struct{}
	entered chan struct{}
	once    sync.Once
	mu      sync.Mutex
	body    strings.Builder
}

func (g *gatedWriter) Header() http.Header { return g.header }
func (g *gatedWriter) WriteHeader(int)     {}
func (g *gatedWriter) Flush()              {}
func (g *gatedWriter) Write(p []byte) (int, error) {
	if strings.HasPrefix(string(p), "data: ") {
		g.once.Do(func() { close(g.entered) })
		<-g.gate
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.body.Write(p)
}

// When the stop closes SSE, the frames already queued for a connection (the chat streams' terminal events, emitted
// just before) are written before the handler returns: with both the queue and the shutdown ready, the handler's
// select would otherwise drop them at random.
func TestHandleEvents_ShutdownWritesQueuedFrames(t *testing.T) {
	shutdownCh := make(chan struct{})
	m := NewEventManager(shutdownCh)
	m.heartbeatInterval = time.Hour
	w := &gatedWriter{header: http.Header{}, gate: make(chan struct{}), entered: make(chan struct{})}
	done := make(chan struct{})
	go func() {
		defer close(done)
		m.HandleEvents(w, httptest.NewRequest(http.MethodGet, "/api/events", nil))
	}()
	for deadline := time.Now().Add(5 * time.Second); !m.HasSubscriber(""); time.Sleep(time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("the event stream never registered")
		}
	}

	m.EmitTo("", "e1", 1)
	<-w.entered // the handler is writing e1; e2 and e3 queue behind it
	m.EmitTo("", "e2", 2)
	m.EmitTo("", "e3", 3)
	close(shutdownCh)
	close(w.gate)
	<-done

	w.mu.Lock()
	body := w.body.String()
	w.mu.Unlock()
	for _, name := range []string{`"e1"`, `"e2"`, `"e3"`} {
		if !strings.Contains(body, name) {
			t.Errorf("event %s was queued before the shutdown but not written; body %q", name, body)
		}
	}
}

// Resuming a stream this server does not know (it restarted without the resume backplane, the retention passed, or
// it is another user's) answers code STREAM_NOT_FOUND, with the status it always had: the client reports the
// stream interrupted at once instead of waiting for its stall probe.
func TestChatResume_UnknownStreamIsStreamNotFound(t *testing.T) {
	check := func(t *testing.T, rr *httptest.ResponseRecorder, status int) {
		t.Helper()
		var env render.ErrorResponse
		_ = json.Unmarshal(rr.Body.Bytes(), &env)
		if rr.Code != status || env.Code != "STREAM_NOT_FOUND" {
			t.Errorf("resume of an unknown stream: %d %s, want %d STREAM_NOT_FOUND", rr.Code, rr.Body.String(), status)
		}
	}
	t.Run("without accounts", func(t *testing.T) {
		rt := newTestRouter(nil, false)
		check(t, doRequest(t, rt, http.MethodPost, "/api/chat/resume", map[string]string{"id": uuid.NewString()}), http.StatusNotFound)
	})
	t.Run("with accounts", func(t *testing.T) {
		rt := newTestRouter(testutil.NewFakeBackend(), true)
		bearer := jwtBearer(t, rt, "u1", "u1@example.com")
		check(t, doRequestWithAuth(t, rt, http.MethodPost, "/api/chat/resume", bearer, map[string]string{"id": uuid.NewString()}), http.StatusForbidden)
	})
}

// R1: the stop hook used to close every SSE connection and only then cancel the chat streams, so the terminal
// event was lost and the client waited 90-120 s for its stall probe. In the order the stop hook runs now (drain,
// end the chat streams, then close SSE) the stream's SERVER_RESTARTING error reaches the open connection before it
// closes.
func TestShutdown_ChatStreamGetsServerRestartingBeforeSSECloses(t *testing.T) {
	shutdownCh := make(chan struct{})
	events := NewEventManager(shutdownCh)
	srv := httptest.NewServer(http.HandlerFunc(events.HandleEvents))
	defer srv.Close()

	p := &blockingStreamProvider{FakeProvider: &testutil.FakeProvider{}, release: make(chan struct{})}
	defer p.closeOnce.Do(func() { close(p.release) })
	ai.RegisterProvider("drain-blocking", func(string) ai.Provider { return p })
	factory := ai.NewProviderFactory(func(string, string) (string, error) { return "k", nil }, nil, nil, nil)
	chatSvc := service.NewChatService(events, t.TempDir(), &service.FlowService{}, &service.AnalysisService{},
		storage.NewInMemorySettingsStore(), factory, nil, config.ModeLocal, service.ChatOptions{})

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, srv.URL, nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	frames := make(chan string, 64)
	go func() {
		defer close(frames)
		sc := bufio.NewScanner(resp.Body)
		for sc.Scan() {
			if line := sc.Text(); strings.HasPrefix(line, "data: ") {
				frames <- line
			}
		}
	}()

	streamID := uuid.NewString()
	if _, err := chatSvc.StreamChatMessage(ctx, service.LocalUser, &models.FlowDocument{ID: "f-drain", Name: "f"}, nil,
		models.ChatRequest{Provider: "drain-blocking", UserMessage: "hi", ClientStreamID: streamID}); err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	for live := false; !live; {
		select {
		case f, ok := <-frames:
			if !ok {
				t.Fatal("the event stream ended before the first chunk")
			}
			live = strings.Contains(f, streamID) && strings.Contains(f, `"type":"chunk"`)
		case <-ctx.Done():
			t.Fatal("no chunk reached the event stream")
		}
	}

	// The stop hook's order: drain (no pause here), end the chat streams, then close SSE (Router.Shutdown).
	if err := drain.New().Drain(ctx, 0, chatSvc.Shutdown); err != nil {
		t.Fatalf("Drain: %v", err)
	}
	close(shutdownCh)

	var got []string
	for f := range frames {
		got = append(got, f)
	}
	for _, f := range got {
		if strings.Contains(f, streamID) && strings.Contains(f, `"type":"error"`) && strings.Contains(f, `"code":"SERVER_RESTARTING"`) {
			return
		}
	}
	t.Fatalf("frames after the stop began %q, want the stream's SERVER_RESTARTING error before the connection closed", got)
}
