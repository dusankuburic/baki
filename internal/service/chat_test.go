package service

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// TestStreamChatMessage_PreStreamFailureFailsFast verifies that a stream whose
// provider can't even be resolved does not park on `<-ctl.started` waiting for
// /api/chat/begin: the goroutine exits immediately, the error is emitted, and
// BeginStream hands the buffered error back for the client that begins late
// (whose SSE subscription didn't exist when the event was emitted).
func TestStreamChatMessage_PreStreamFailureFailsFast(t *testing.T) {
	notifier := &testutil.CountingNotifier{}

	factory := ai.NewProviderFactory(
		func(scope, provider string) (string, error) {
			return "", fmt.Errorf("no key configured")
		},
		nil,
		nil,
		nil,
	)

	svc := &ChatService{
		notifier:      notifier,
		flowCache:     &FlowService{},
		analysisCache: &AnalysisService{},
		factory:       factory,
	}

	id, err := svc.StreamChatMessage(context.Background(), "test", nil, nil, models.ChatRequest{Provider: "unknown"})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}

	// No begin, no cancel: the goroutine must release itself.
	deadline := time.After(2 * time.Second)
	for {
		if _, ok := svc.activeStreams.Load(id); !ok {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("stream goroutine did not exit within 2s without begin/cancel")
		case <-time.After(10 * time.Millisecond):
		}
	}

	if got := notifier.Count(); got == 0 {
		t.Error("expected the pre-stream error to be emitted immediately, got 0 events")
	}
	res := svc.BeginStream(context.Background(), id)
	if res == nil || res.Error == "" {
		t.Fatalf("BeginStream after fail-fast = %+v, want finished state with error", res)
	}
}

// TestStreamChatMessage_ParentCancelBeforeBegin_StillRuns is a regression test
// for the "context canceled" bug: the create-request's context (r.Context())
// must NOT abort the stream, since the stream outlives that request (begin is a
// separate call). We cancel the parent immediately — as net/http does when the
// create handler returns — then begin the stream and assert it still ran (an
// error event is emitted). Before the fix (stream ctx derived from r.Context()),
// the parent cancel propagated, awaitStart() saw ctx.Done() and bailed, so no
// event was emitted; this manifested as Copilot's token exchange failing with
// "context canceled" on a cold token cache.
func TestStreamChatMessage_ParentCancelBeforeBegin_StillRuns(t *testing.T) {
	notifier := &testutil.CountingNotifier{}

	factory := ai.NewProviderFactory(
		func(scope, provider string) (string, error) {
			return "", fmt.Errorf("no key configured")
		},
		nil,
		nil,
		nil,
	)

	svc := &ChatService{
		notifier:      notifier,
		flowCache:     &FlowService{},
		analysisCache: &AnalysisService{},
		factory:       factory,
	}

	// Simulate the HTTP create handler: it passes its request context, then
	// returns — which cancels that context.
	parent, cancelParent := context.WithCancel(context.Background())
	id, err := svc.StreamChatMessage(parent, "test", nil, nil, models.ChatRequest{Provider: "unknown"})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	cancelParent()

	svc.BeginStream(context.Background(), id)

	deadline := time.After(2 * time.Second)
	for {
		if _, ok := svc.activeStreams.Load(id); !ok {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("stream goroutine did not exit within 2s after BeginStream")
		case <-time.After(10 * time.Millisecond):
		}
	}

	if got := notifier.Count(); got == 0 {
		t.Errorf("expected an error event after BeginStream despite parent cancel, got 0 (stream context inherited request cancellation)")
	}
}

// TestStreamChatMessage_ClientStreamID_AutoBegins verifies the C-1 handshake:
// a client-provided stream ID is used as the stream identity and the stream is
// auto-begun (ctl.started closed immediately) so the worker may emit without a
// /chat/begin round-trip. The legacy path (no ClientStreamID) leaves started
// open for BeginStream. Invalid IDs and collisions are rejected.
func TestStreamChatMessage_ClientStreamID_AutoBegins(t *testing.T) {
	notifier := &testutil.CountingNotifier{}
	// Block provider resolution so the worker parks and the stream stays in
	// activeStreams long enough to inspect ctl.started without racing cleanup.
	release := make(chan struct{})
	factory := ai.NewProviderFactory(func(_, _ string) (string, error) {
		<-release
		return "", fmt.Errorf("test: provider released")
	}, nil, nil, nil)
	svc := &ChatService{notifier: notifier, flowCache: &FlowService{}, analysisCache: &AnalysisService{}, factory: factory}

	// 1. A valid client-provided UUID is used as the id and auto-begins.
	sid := "12345678-1234-5678-1234-567812345678"
	id, err := svc.StreamChatMessage(context.Background(), "test", nil, nil, models.ChatRequest{Provider: "claude", ClientStreamID: sid})
	if err != nil {
		t.Fatalf("StreamChatMessage with client id: %v", err)
	}
	if id != sid {
		t.Fatalf("expected client-provided stream id %q, got %q", sid, id)
	}
	ctlVal, ok := svc.activeStreams.Load(id)
	if !ok {
		t.Fatal("client-id stream not found in activeStreams")
	}
	ctl := ctlVal.(*streamCtl)
	select {
	case <-ctl.started:
		// good — auto-begun without BeginStream
	default:
		t.Error("expected ctl.started to be closed immediately for a client-provided stream id (C-1 auto-begin)")
	}

	// 2. Legacy path (no ClientStreamID) leaves ctl.started open for BeginStream.
	id2, err := svc.StreamChatMessage(context.Background(), "test", nil, nil, models.ChatRequest{Provider: "claude"})
	if err != nil {
		t.Fatalf("StreamChatMessage legacy: %v", err)
	}
	ctl2Val, ok := svc.activeStreams.Load(id2)
	if !ok {
		t.Fatal("legacy stream not found in activeStreams")
	}
	ctl2 := ctl2Val.(*streamCtl)
	select {
	case <-ctl2.started:
		t.Error("legacy stream must NOT auto-begin; ctl.started should remain open until BeginStream")
	default:
		// good
	}

	// 3. Invalid (non-UUID) clientStreamId is rejected.
	if _, err := svc.StreamChatMessage(context.Background(), "test", nil, nil, models.ChatRequest{Provider: "claude", ClientStreamID: "not-a-uuid"}); err == nil {
		t.Error("expected an error for a non-UUID clientStreamId")
	}

	// 4. Collision with an active stream's id is rejected.
	if _, err := svc.StreamChatMessage(context.Background(), "test", nil, nil, models.ChatRequest{Provider: "claude", ClientStreamID: sid}); err == nil {
		t.Error("expected an error for a colliding clientStreamId")
	}

	// Release the parked workers so they fail-fast and clean up (no goroutine leak).
	close(release)
	for _, s := range []string{id, id2} {
		deadline := time.After(2 * time.Second)
		for {
			if _, ok := svc.activeStreams.Load(s); !ok {
				break
			}
			select {
			case <-deadline:
				t.Fatalf("stream %s did not exit within 2s after release", s)
			case <-time.After(10 * time.Millisecond):
			}
		}
	}
}

// TestStreamChatMessage_CancelAfterBegin_EmitsError verifies the normal
// cancellation path.
func TestStreamChatMessage_CancelAfterBegin_EmitsError(t *testing.T) {
	notifier := &testutil.CountingNotifier{}

	factory := ai.NewProviderFactory(
		func(scope, provider string) (string, error) {
			return "", fmt.Errorf("no key configured")
		},
		nil,
		nil,
		nil,
	)

	svc := &ChatService{
		notifier:      notifier,
		flowCache:     &FlowService{},
		analysisCache: &AnalysisService{},
		factory:       factory,
	}

	id, err := svc.StreamChatMessage(context.Background(), "test", nil, nil, models.ChatRequest{Provider: "unknown"})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}

	svc.BeginStream(context.Background(), id)

	deadline := time.After(2 * time.Second)
	for {
		if _, ok := svc.activeStreams.Load(id); !ok {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("stream goroutine did not exit within 2s after BeginStream")
		case <-time.After(10 * time.Millisecond):
		}
	}

	if got := notifier.Count(); got == 0 {
		t.Errorf("expected at least one error event after BeginStream, got 0")
	}
}

func TestBeginStream_ConcurrentCalls_NoPanic(t *testing.T) {
	notifier := &testutil.CountingNotifier{}
	factory := ai.NewProviderFactory(
		func(scope, provider string) (string, error) { return "", fmt.Errorf("no key") },
		nil,
		nil,
		nil,
	)
	svc := &ChatService{
		notifier:      notifier,
		flowCache:     &FlowService{},
		analysisCache: &AnalysisService{},
		factory:       factory,
	}

	id, err := svc.StreamChatMessage(context.Background(), "test", nil, nil, models.ChatRequest{Provider: "unknown"})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}

	const N = 50
	done := make(chan struct{}, N)
	for range N {
		go func() {
			defer func() {
				if rec := recover(); rec != nil {
					t.Errorf("BeginStream panicked: %v", rec)
				}
				done <- struct{}{}
			}()
			svc.BeginStream(context.Background(), id)
		}()
	}
	for range N {
		<-done
	}

	svc.CancelStream(id)
	deadline := time.After(2 * time.Second)
	for {
		if _, ok := svc.activeStreams.Load(id); !ok {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("stream goroutine did not exit within 2s")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// TestWatchStream_CancelsWhenSubscriberGone verifies the stream watchdog:
// once the client has begun and its SSE connection stays gone for the miss
// limit, the stream is cancelled instead of billing until the wall-clock cap.
func TestWatchStream_CancelsWhenSubscriberGone(t *testing.T) {
	notifier := &testutil.CountingNotifier{}
	notifier.SetNoSubscriber(true)
	svc := &ChatService{notifier: notifier, watchdogInterval: 5 * time.Millisecond, idleTimeout: time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctl := &streamCtl{cancel: cancel, started: make(chan struct{})}
	ctl.touch()
	close(ctl.started) // client has begun

	go svc.watchStream(ctx, "s1", "user-1", ctl)

	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not cancel the stream after subscriber loss")
	}
}

// TestWatchStream_LiveSubscriberNotCancelled: a connected client with an
// active provider must never have its stream cancelled by the watchdog.
func TestWatchStream_LiveSubscriberNotCancelled(t *testing.T) {
	notifier := &testutil.CountingNotifier{}
	svc := &ChatService{notifier: notifier, watchdogInterval: 5 * time.Millisecond, idleTimeout: time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctl := &streamCtl{cancel: cancel, started: make(chan struct{})}
	ctl.touch()
	close(ctl.started)

	go svc.watchStream(ctx, "s1", "user-1", ctl)

	select {
	case <-ctx.Done():
		t.Fatal("watchdog cancelled a stream whose subscriber is connected")
	case <-time.After(100 * time.Millisecond): // many ticks worth
	}
}

// TestWatchStream_NotStartedNotCancelledBySubscriberLoss: before the client
// begins, subscriber liveness must not cancel — pre-begin lifetime is bounded
// by fail-fast errors, the idle timeout, and the stream cap.
func TestWatchStream_NotStartedNotCancelledBySubscriberLoss(t *testing.T) {
	notifier := &testutil.CountingNotifier{}
	notifier.SetNoSubscriber(true)
	svc := &ChatService{notifier: notifier, watchdogInterval: 5 * time.Millisecond, idleTimeout: time.Hour}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctl := &streamCtl{cancel: cancel, started: make(chan struct{})} // never closed
	ctl.touch()

	go svc.watchStream(ctx, "s1", "user-1", ctl)

	select {
	case <-ctx.Done():
		t.Fatal("watchdog cancelled a stream the client never began")
	case <-time.After(100 * time.Millisecond):
	}
}

// TestWatchStream_CancelsIdleProvider: a provider that stops emitting chunks
// is cancelled after the idle timeout — even before the client begins — with
// the provider-stalled reason.
func TestWatchStream_CancelsIdleProvider(t *testing.T) {
	notifier := &testutil.CountingNotifier{}
	svc := &ChatService{notifier: notifier, watchdogInterval: 5 * time.Millisecond, idleTimeout: 30 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctl := &streamCtl{cancel: cancel, started: make(chan struct{})} // not begun
	ctl.touch()

	go svc.watchStream(ctx, "s1", "user-1", ctl)

	select {
	case <-ctx.Done():
		if got := ctl.failureMessage(ctx, context.Canceled); got != "response stopped: the AI provider stopped responding" {
			t.Errorf("failureMessage = %q, want the provider-stalled reason", got)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog did not cancel an idle provider stream")
	}
}

// TestWatchStream_ActiveProviderNotIdleCancelled: steady provider chunks keep
// the stream alive well past the idle timeout.
func TestWatchStream_ActiveProviderNotIdleCancelled(t *testing.T) {
	notifier := &testutil.CountingNotifier{}
	svc := &ChatService{notifier: notifier, watchdogInterval: 5 * time.Millisecond, idleTimeout: 40 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctl := &streamCtl{cancel: cancel, started: make(chan struct{})}
	ctl.touch()
	close(ctl.started)

	go svc.watchStream(ctx, "s1", "user-1", ctl)
	stop := time.After(200 * time.Millisecond) // 5× the idle timeout
	for {
		select {
		case <-ctx.Done():
			t.Fatal("watchdog idle-cancelled a stream with steady provider activity")
		case <-stop:
			return
		case <-time.After(10 * time.Millisecond):
			ctl.touch()
		}
	}
}

// TestWatchStream_ToolExecutionTouchedNotIdleCancelled locks the contract the
// tool loops rely on: tool execution emits no provider chunks, so the loops
// ctl.touch() around ExecuteTool (chat.go runToolLoop/runPromptToolLoop) to
// signal activity. A stream whose only activity is these periodic touches —
// simulating a legitimately slow tool running well past the idle timeout —
// must NOT be cancelled as an idle provider.
func TestWatchStream_ToolExecutionTouchedNotIdleCancelled(t *testing.T) {
	notifier := &testutil.CountingNotifier{}
	svc := &ChatService{notifier: notifier, watchdogInterval: 5 * time.Millisecond, idleTimeout: 40 * time.Millisecond}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	ctl := &streamCtl{cancel: cancel, started: make(chan struct{})}
	ctl.touch()
	close(ctl.started)

	go svc.watchStream(ctx, "s1", "user-1", ctl)

	// Simulate a slow tool: 12 "tool execution" windows of 20ms each — every
	// window well under the 40ms idle limit, but the total (240ms) far past
	// it. Each window starts and ends with a touch, exactly like the loop.
	stop := time.After(2 * time.Second)
	for i := 0; i < 12; i++ {
		select {
		case <-ctx.Done():
			t.Fatalf("watchdog idle-cancelled during slow tool execution (window %d)", i)
		case <-stop:
			t.Fatal("test timed out")
		default:
		}
		ctl.touch() // loop touches before ExecuteTool
		time.Sleep(20 * time.Millisecond)
		ctl.touch() // …and after
	}
	// Sanity contrast: with touches stopped, the same stream IS idle-cancelled
	// (proves the touches above were load-bearing, not a short test).
	select {
	case <-ctx.Done():
	case <-time.After(2 * time.Second):
		t.Fatal("watchdog failed to cancel once activity stopped — contrast half of the test is vacuous")
	}
}

// windowFakeProvider scripts one plain (tool-less) assistant turn like
// testutil.FakeProvider, but with a controllable catalog: Models() lists
// "big-window" with a huge ContextLimit while the provider-wide ContextLimit()
// stays small, and Stream records how many messages each request carried.
type windowFakeProvider struct {
	testutil.FakeProvider
	providerLimit int
	streamMsgs    []int
}

func (p *windowFakeProvider) Models(_ context.Context) ([]ai.ModelInfo, error) {
	return []ai.ModelInfo{{ID: "big-window", ContextLimit: 1 << 20}}, nil
}
func (p *windowFakeProvider) ContextLimit() int { return p.providerLimit }
func (p *windowFakeProvider) Stream(ctx context.Context, req ai.Request, onChunk func(ai.Chunk)) error {
	p.streamMsgs = append(p.streamMsgs, len(req.Messages))
	return p.FakeProvider.Stream(ctx, req, onChunk)
}

// TestRunToolLoop_UsesModelCatalogWindow proves the tool loop truncates
// against the SELECTED MODEL's catalog window, not the smaller provider-wide
// default: the same conversation that fails as "too long" under the provider
// default streams intact (all history delivered to the provider) once the
// model's catalog entry advertises a window big enough to hold it.
func TestRunToolLoop_UsesModelCatalogWindow(t *testing.T) {
	newSvc := func() *ChatService {
		return &ChatService{notifier: &testutil.CountingNotifier{}, analysisCache: &AnalysisService{}}
	}

	// A pinned first turn of ~1500 tokens (6000 bytes at the fake's len/4
	// estimator) plus the 4000 reserve overflows a 5000 provider default on
	// its own (→ ErrContextLimit even after truncation), while the 1M catalog
	// window holds the whole conversation.
	msgs := []ai.Message{{Role: "user", Content: strings.Repeat("u", 6000)}}
	for i := 0; i < 20; i++ {
		role := "assistant"
		if i%2 == 1 {
			role = "user"
		}
		msgs = append(msgs, ai.Message{Role: role, Content: fmt.Sprintf("turn %d %s", i, strings.Repeat("x", 300))})
	}

	run := func(model string) (provider *windowFakeProvider, ctl *streamCtl) {
		provider = &windowFakeProvider{
			FakeProvider:  testutil.FakeProvider{Tools: true, Turns: []testutil.FakeTurn{{Text: "ok"}}},
			providerLimit: 5000,
		}
		ctl = &streamCtl{cancel: func() {}, started: make(chan struct{})}
		close(ctl.started)
		svc := newSvc()
		svc.runToolLoop(context.Background(), provider, ai.Request{
			SystemPrompt: "sys",
			Model:        model,
			Messages:     msgs,
		}, "user-1", &ai.ToolContext{Ctx: context.Background()}, ctl, func() bool { return true }, func(string, map[string]interface{}) {}, func() int { return 0 })
		return provider, ctl
	}

	bigProv, bigCtl := run("big-window")
	if bigCtl.errMsg != "" {
		t.Fatalf("catalog window must hold the conversation, got failure %q", bigCtl.errMsg)
	}
	if len(bigProv.streamMsgs) != 1 || bigProv.streamMsgs[0] != len(msgs) {
		t.Errorf("provider must receive the full history (%d messages) under the catalog window, got %v", len(msgs), bigProv.streamMsgs)
	}

	_, smallCtl := run("not-in-catalog")
	if smallCtl.errMsg == "" {
		t.Fatal("uncatalogued model falls back to the small provider default — expected a 'conversation too long' failure")
	}
	if !strings.Contains(smallCtl.errMsg, "too long") {
		t.Errorf("want context-window failure message, got %q", smallCtl.errMsg)
	}
}

// TestFailureMessage maps stream failures to client-facing text: a deliberate
// cancellation surfaces its stored reason instead of the provider's raw
// "context canceled" wrapping, the duration cap gets a readable message, and
// genuine provider errors pass through untouched.
func TestFailureMessage(t *testing.T) {
	t.Run("cancel reason replaces raw error", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		ctl := &streamCtl{cancel: cancel}
		ctl.cancelWithReason("response stopped: you were disconnected while it was generating")
		got := ctl.failureMessage(ctx, fmt.Errorf("reading openai SSE stream: %w", context.Canceled))
		if got != "response stopped: you were disconnected while it was generating" {
			t.Errorf("failureMessage = %q, want the stored cancel reason", got)
		}
	})

	t.Run("first cancel reason wins", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		ctl := &streamCtl{cancel: cancel}
		ctl.cancelWithReason("first")
		ctl.cancelWithReason("second")
		if got := ctl.failureMessage(ctx, context.Canceled); got != "first" {
			t.Errorf("failureMessage = %q, want %q", got, "first")
		}
	})

	t.Run("deadline gets readable message", func(t *testing.T) {
		ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
		defer cancel()
		ctl := &streamCtl{cancel: cancel}
		got := ctl.failureMessage(ctx, context.DeadlineExceeded)
		if got != "response stopped: maximum response time reached" {
			t.Errorf("failureMessage = %q, want the max-duration message", got)
		}
	})

	t.Run("provider error passes through on live context", func(t *testing.T) {
		ctl := &streamCtl{cancel: func() {}}
		got := ctl.failureMessage(context.Background(), fmt.Errorf("upstream exploded"))
		if got != "upstream exploded" {
			t.Errorf("failureMessage = %q, want the raw error", got)
		}
	})
}

func TestNormalizeChatParams(t *testing.T) {
	cases := []struct {
		name       string
		temp       float64
		maxTok     int
		ctxLimit   int
		maxOutput  int
		wantTemp   float64
		wantMaxTok int
	}{
		{"in range untouched", 0.7, 1000, 128000, 0, 0.7, 1000},
		{"temp below zero clamps to 0", -1, 100, 0, 0, 0, 100},
		{"temp above two clamps to 2", 5, 100, 0, 0, 2, 100},
		{"negative maxtokens clamps to 0", 0.5, -10, 0, 0, 0.5, 0},
		{"maxtokens over context window is capped", 0.5, 999999, 8000, 0, 0.5, 8000 - contextReserve},
		{"unknown ctxlimit leaves maxtokens", 0.5, 999999, 0, 0, 0.5, 999999},
		{"tiny context window floors cap at 0", 0.5, 100, 1000, 0, 0.5, 0},
		{"output cap bounds maxtokens below input window", 0.5, 200000, 200000, 64000, 0.5, 64000},
		{"output cap ignored when maxtokens already under it", 0.5, 1000, 200000, 64000, 0.5, 1000},
		{"unknown output cap falls back to context backstop", 0.5, 999999, 8000, 0, 0.5, 8000 - contextReserve},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			gotTemp, gotMaxTok := normalizeChatParams(c.temp, c.maxTok, c.ctxLimit, c.maxOutput)
			if gotTemp != c.wantTemp {
				t.Errorf("temp = %v, want %v", gotTemp, c.wantTemp)
			}
			if gotMaxTok != c.wantMaxTok {
				t.Errorf("maxTokens = %v, want %v", gotMaxTok, c.wantMaxTok)
			}
		})
	}
}

// Tool-loop tests use the shared testutil.FakeProvider (scriptable streamed
// turns + context-window guard). A turn with no scripted entry and a Fallback
// set drives the iteration-cap path.

func toolLoopDoc() *models.FlowDocument {
	doc := &models.FlowDocument{
		ID: "f1",
		Subflows: []models.Subflow{{
			ID: "sf1", Name: "Main",
			Blocks: []models.Block{{ID: "b1", Name: "Xavier", Type: models.BlockTypeAction, RawType: "Foo.Bar"}},
		}},
	}
	doc.RebuildIndexes()
	return doc
}

func collectEvents() (func(string, map[string]interface{}), *[]string) {
	var evs []string
	return func(typ string, _ map[string]interface{}) { evs = append(evs, typ) }, &evs
}

// TestDailyUsage_NilBackendDoesNotPanic covers the local/desktop-mode case where
// the storage backend is nil (the default StorageLocal builds no backend): the
// budget check must report $0 usage instead of dereferencing a nil backend.
func TestDailyUsage_NilBackendDoesNotPanic(t *testing.T) {
	svc := &ChatService{} // backend is nil, as in local mode
	usage, err := svc.dailyUsage(context.Background(), "user-1", "org-1")
	if err != nil {
		t.Fatalf("dailyUsage with nil backend returned error: %v", err)
	}
	if usage != 0 {
		t.Errorf("dailyUsage with nil backend = %v, want 0", usage)
	}
}

// staticSettings is a minimal SettingsProvider for service-layer tests that need
// to drive settings without the on-disk SettingsStore.
type staticSettings struct{ s models.AppSettings }

func (m *staticSettings) Get() *models.AppSettings          { cp := m.s; return &cp }
func (m *staticSettings) Update(models.AppSettings) error   { return nil }
func (m *staticSettings) AddRecentFile(string, int64) error { return nil }
func (m *staticSettings) RemoveRecentFile(string) error     { return nil }
func (m *staticSettings) ClearRecentFiles() error           { return nil }

// TestEnforceBudget_FailsClosedOnStoreError verifies the cost guardrail denies
// a request when the day's spend can't be read, instead of treating the unknown
// spend as $0 (which would open an unlimited-spend window during a DB hiccup).
func TestEnforceBudget_FailsClosedOnStoreError(t *testing.T) {
	backend := &testutil.FakeBackend{UsageErr: fmt.Errorf("db down")}
	svc := &ChatService{
		backend:  backend,
		settings: &staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: 10}}},
	}
	if err := svc.enforceBudget(context.Background(), "user-1", "org-1", 0); err == nil {
		t.Fatal("enforceBudget with a store error returned nil, want a fail-closed error")
	}
}

// TestEnforceBudget_AllowsUnderBudget confirms the happy path still permits a
// request when verified spend is below the configured budget.
func TestEnforceBudget_AllowsUnderBudget(t *testing.T) {
	backend := &testutil.FakeBackend{DailyUsage: 2}
	svc := &ChatService{
		backend:  backend,
		settings: &staticSettings{s: models.AppSettings{AI: models.AISettings{DailyBudget: 10}}},
	}
	if err := svc.enforceBudget(context.Background(), "user-1", "org-1", 0); err != nil {
		t.Fatalf("enforceBudget under budget returned error: %v", err)
	}
}

func TestRunToolLoop_ExecutesToolThenFinal(t *testing.T) {
	stub := &testutil.FakeProvider{Turns: []testutil.FakeTurn{
		{ToolCalls: []ai.ToolCall{{ID: "t1", Name: "search_flow", Input: []byte(`{"query":"xav"}`)}}, TokensIn: 10, TokensOut: 5},
		{Text: "Final answer", TokensIn: 8, TokensOut: 3},
	}}
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := &streamCtl{}
	emit, evs := collectEvents()

	svc.runToolLoop(context.Background(), stub, ai.Request{Messages: []ai.Message{{Role: "user", Content: "hi"}}},
		"user-1", &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, ctl, func() bool { return true }, emit, func() int { return 0 })

	if stub.Calls() != 2 {
		t.Fatalf("expected 2 Stream calls, got %d", stub.Calls())
	}
	// tool (starting) → tool_result (finished, transparency trail) → final.
	if got := strings.Join(*evs, ","); got != "tool,tool_result,chunk,done" {
		t.Errorf("expected tool,tool_result,chunk,done events, got %q", got)
	}
	if !ctl.done || ctl.buffer.String() != "Final answer" {
		t.Errorf("ctl not finalized: done=%v buffer=%q", ctl.done, ctl.buffer.String())
	}
	if ctl.tokensIn != 18 || ctl.tokensOut != 8 {
		t.Errorf("tokens not summed across turns: in=%d out=%d", ctl.tokensIn, ctl.tokensOut)
	}
}

func TestRunToolLoop_NoToolsFinalImmediately(t *testing.T) {
	stub := &testutil.FakeProvider{Turns: []testutil.FakeTurn{{Text: "Direct", TokensIn: 1, TokensOut: 2}}}
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := &streamCtl{}
	emit, evs := collectEvents()

	svc.runToolLoop(context.Background(), stub, ai.Request{}, "user-1", &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, ctl, func() bool { return true }, emit, func() int { return 0 })

	if stub.Calls() != 1 {
		t.Fatalf("expected 1 Stream call, got %d", stub.Calls())
	}
	// Two chunk events: "Direct" ends in "t" — a possible prefix of the
	// "token" secret anchor — so the output scrubber holds that byte until the
	// turn-end flush proves it is prose.
	if got := strings.Join(*evs, ","); got != "chunk,chunk,done" {
		t.Errorf("expected chunk,chunk,done, got %q", got)
	}
	if ctl.buffer.String() != "Direct" {
		t.Errorf("buffer = %q, want %q", ctl.buffer.String(), "Direct")
	}
}

// TestRunToolLoop_MasksSecretSplitAcrossChunks: model output containing a
// secret — even one split across stream chunks — must reach the client (and
// the resume buffer) masked.
func TestRunToolLoop_MasksSecretSplitAcrossChunks(t *testing.T) {
	stub := &testutil.FakeProvider{Turns: []testutil.FakeTurn{{Parts: []string{"the flow uses password=sup", "ersecret123 in Database.Connect"}}}}
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := &streamCtl{}

	var streamed strings.Builder
	emit := func(typ string, data map[string]interface{}) {
		if typ == "chunk" {
			streamed.WriteString(data["content"].(string))
		}
	}

	svc.runToolLoop(context.Background(), stub, ai.Request{}, "user-1", &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, ctl, func() bool { return true }, emit, func() int { return 0 })

	for name, got := range map[string]string{"emitted chunks": streamed.String(), "resume buffer": ctl.buffer.String()} {
		if strings.Contains(got, "supersecret123") {
			t.Errorf("%s leaked the split secret: %q", name, got)
		}
		if !strings.Contains(got, "[REDACTED]") {
			t.Errorf("%s did not mask the secret: %q", name, got)
		}
	}
}

func TestRunToolLoop_ChatErrorEmitsError(t *testing.T) {
	stub := &testutil.FakeProvider{Err: fmt.Errorf("boom")}
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := &streamCtl{}
	emit, evs := collectEvents()

	svc.runToolLoop(context.Background(), stub, ai.Request{}, "user-1", &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, ctl, func() bool { return true }, emit, func() int { return 0 })

	if got := strings.Join(*evs, ","); got != "error" {
		t.Errorf("expected single error event, got %q", got)
	}
	if ctl.errMsg == "" {
		t.Error("expected ctl.errMsg set")
	}
}

func TestRunToolLoop_IterationCap(t *testing.T) {
	stub := &testutil.FakeProvider{Fallback: &testutil.FakeTurn{ToolCalls: []ai.ToolCall{{ID: "t", Name: "search_flow", Input: []byte(`{"query":"x"}`)}}}} // never terminates
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := &streamCtl{}
	emit, evs := collectEvents()

	svc.runToolLoop(context.Background(), stub, ai.Request{}, "user-1", &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, ctl, func() bool { return true }, emit, func() int { return 0 })

	if stub.Calls() != maxToolIterations {
		t.Errorf("expected %d Stream calls at cap, got %d", maxToolIterations, stub.Calls())
	}
	last := (*evs)[len(*evs)-1]
	if last != "error" {
		t.Errorf("expected final error event at iteration cap, got %q", last)
	}
}

func TestRunToolLoop_NotStartedEmitsNothing(t *testing.T) {
	stub := &testutil.FakeProvider{Turns: []testutil.FakeTurn{{Text: "x"}}}
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := &streamCtl{}
	emit, evs := collectEvents()

	// awaitStart returns false (client cancelled before begin) → no events.
	svc.runToolLoop(context.Background(), stub, ai.Request{}, "user-1", &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, ctl, func() bool { return false }, emit, func() int { return 0 })

	if len(*evs) != 0 {
		t.Errorf("expected no events when not started, got %v", *evs)
	}
}

// TestRunToolLoop_EmitsToolResult pins the tool_result transparency event: one
// per executed tool, after execution, carrying name/label, ok (per
// ExecuteTool's "error:" contract), a non-negative duration, and a one-line
// summary. The failing half drives an unknown tool through the same path.
func TestRunToolLoop_EmitsToolResult(t *testing.T) {
	cases := []struct {
		name       string
		tool       string
		input      []byte
		wantOK     bool
		wantSubstr string
	}{
		{"ok tool", "search_flow", []byte(`{"query":"xav"}`), true, ""},
		{"failing tool", "no_such_tool", []byte(`{}`), false, "error: unknown tool"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			stub := &testutil.FakeProvider{Turns: []testutil.FakeTurn{
				{ToolCalls: []ai.ToolCall{{ID: "t1", Name: tc.tool, Input: tc.input}}, TokensIn: 1, TokensOut: 1},
				{Text: "done", TokensIn: 1, TokensOut: 1},
			}}
			svc := &ChatService{analysisCache: &AnalysisService{}}
			ctl := &streamCtl{}

			var got map[string]interface{}
			emit := func(typ string, data map[string]interface{}) {
				if typ == "tool_result" {
					got = data
				}
			}

			svc.runToolLoop(context.Background(), stub, ai.Request{}, "user-1", &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, ctl, func() bool { return true }, emit, func() int { return 0 })

			if got == nil {
				t.Fatal("no tool_result event emitted")
			}
			if got["name"] != tc.tool {
				t.Errorf("name = %v, want %q", got["name"], tc.tool)
			}
			if label, _ := got["label"].(string); label == "" {
				t.Error("label missing in tool_result payload")
			}
			if ok, _ := got["ok"].(bool); ok != tc.wantOK {
				t.Errorf("ok = %v, want %v", ok, tc.wantOK)
			}
			if ms, _ := got["durationMs"].(int64); ms < 0 {
				t.Errorf("durationMs = %v, want >= 0", ms)
			}
			summary, _ := got["summary"].(string)
			if tc.wantSubstr != "" && !strings.Contains(summary, tc.wantSubstr) {
				t.Errorf("summary = %q, want substring %q", summary, tc.wantSubstr)
			}
			if strings.ContainsAny(summary, "\n") {
				t.Errorf("summary must be one line, got %q", summary)
			}
		})
	}
}

// toolsUnsupportedProvider fails the FIRST Stream call (when tools are still
// attached) with the wrapped sentinel, then answers normally — proving the
// runToolLoop degradation path: tools dropped, notice emitted, same iteration
// retried, final answer delivered.
type toolsUnsupportedProvider struct {
	testutil.FakeProvider
	sawTools bool
	calls    int
}

func (p *toolsUnsupportedProvider) Stream(ctx context.Context, req ai.Request, onChunk func(ai.Chunk)) error {
	p.calls++
	if len(req.Tools) > 0 {
		p.sawTools = true
		return fmt.Errorf("wrapped: %w", ai.ErrToolsUnsupported)
	}
	return p.FakeProvider.Stream(ctx, req, onChunk)
}

func TestRunToolLoop_ToolsUnsupportedDegradesGracefully(t *testing.T) {
	stub := &toolsUnsupportedProvider{FakeProvider: testutil.FakeProvider{
		Turns: []testutil.FakeTurn{{Text: "answer without tools", TokensIn: 5, TokensOut: 5}},
		Tools: true,
	}}
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := &streamCtl{}

	var events []string
	var toolLabels []string
	emit := func(typ string, data map[string]interface{}) {
		events = append(events, typ)
		if typ == "tool" {
			if l, ok := data["label"].(string); ok {
				toolLabels = append(toolLabels, l)
			}
		}
	}

	svc.runToolLoop(context.Background(), stub, ai.Request{Messages: []ai.Message{{Role: "user", Content: "hi"}}},
		"user-1", &ai.ToolContext{Ctx: context.Background(), Doc: toolLoopDoc()}, ctl, func() bool { return true }, emit, func() int { return 0 })

	if !stub.sawTools {
		t.Fatal("expected the first attempt to carry tools")
	}
	// Two chunk events: "answer without tools" ends in "s" — a possible
	// prefix of a secret anchor — so the scrubber holds that byte until the
	// turn-end flush (same as TestRunToolLoop_NoToolsFinalImmediately).
	if got := strings.Join(events, ","); got != "tool,chunk,chunk,done" {
		t.Errorf("expected tool(notice),chunk,chunk,done events, got %q", got)
	}
	if len(toolLabels) != 1 || !strings.Contains(toolLabels[0], "answering without") {
		t.Errorf("expected one degradation notice label, got %v", toolLabels)
	}
	if !ctl.done || ctl.buffer.String() != "answer without tools" {
		t.Errorf("turn not finalized: done=%v buffer=%q", ctl.done, ctl.buffer.String())
	}
	// Exactly 2 Stream calls: tools-rejected + toolless answer (the retry did
	// NOT consume an iteration or loop extra times).
	if stub.calls != 2 {
		t.Errorf("expected 2 Stream calls, got %d", stub.calls)
	}
}

// TestToolSummary pins the summary derivation: first line only, whitespace
// trimmed, capped at 120 bytes on a rune boundary with an ellipsis (multibyte
// safety mirrors ai.truncateResult).
func TestToolSummary(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"plain", "3 matches found", "3 matches found"},
		{"first line only", "3 matches found\nand more detail", "3 matches found"},
		{"trims whitespace", "  hello  \nnext", "hello"},
		{"short passthrough", "x", "x"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := toolSummary(tc.in); got != tc.want {
				t.Errorf("toolSummary(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
	t.Run("caps long lines without splitting runes", func(t *testing.T) {
		// 130 multibyte runes (390 bytes) — the cut must land on a boundary.
		in := strings.Repeat("€", 130)
		got := toolSummary(in)
		if !utf8.ValidString(got) {
			t.Fatalf("summary is not valid UTF-8: %q", got)
		}
		if len(got) > 124 { // 120-byte cap + "…"
			t.Errorf("summary too long: %d bytes", len(got))
		}
		if want := strings.TrimSuffix(got, "…"); strings.Count(want, "€") != len(want)/3 {
			t.Errorf("summary split a rune: %q", got)
		}
	})
}

// ctxCacheStubProvider is a minimal ai.Provider for the context-cache test:
// only ID/EstimateTokens/ContextLimit are reachable from cachedContextCore →
// ai.BuildContext (which calls EstimateTokens). Embedding the interface leaves
// the rest nil; unused methods are never invoked on this path.
type ctxCacheStubProvider struct {
	ai.Provider
}

func (ctxCacheStubProvider) ID() string                  { return "stub" }
func (ctxCacheStubProvider) EstimateTokens(s string) int { return len(s) / 4 }
func (ctxCacheStubProvider) ContextLimit() int           { return 100000 }

// TestCachedContextCore_CachesAcrossTurns verifies the scrubbed-context LRU:
// consecutive identical turns reuse the same redacted clone (pointer identity),
// a different cache key rebuilds, and InvalidateChatContext forces a miss.
func TestCachedContextCore_CachesAcrossTurns(t *testing.T) {
	svc := &ChatService{chatCtxCache: newChatContextCache()}
	doc := toolLoopDoc()
	provider := ctxCacheStubProvider{}
	req := models.ChatRequest{Model: "m1", UserMessage: "turn 1"}

	cv1 := svc.cachedContextCore(context.Background(), "scope-1", provider, doc, nil, req)
	cv2 := svc.cachedContextCore(context.Background(), "scope-1", provider, doc, nil, req)
	if cv1.scrubbedDoc != cv2.scrubbedDoc {
		t.Fatal("expected the cached scrubbed doc to be reused (same pointer) on the second identical call")
	}

	// Invalidation bumps the per-flow generation → next call is a miss.
	svc.InvalidateChatContext(doc.ID)
	cv3 := svc.cachedContextCore(context.Background(), "scope-1", provider, doc, nil, req)
	if cv3.scrubbedDoc == cv1.scrubbedDoc {
		t.Fatal("expected a fresh scrubbed doc after InvalidateChatContext")
	}

	// A different cache key (contextBlockID) must not reuse cv1's clone.
	reqBlock := req
	reqBlock.ContextBlockID = "b1"
	cv4 := svc.cachedContextCore(context.Background(), "scope-1", provider, doc, nil, reqBlock)
	if cv4.scrubbedDoc == cv1.scrubbedDoc {
		t.Fatal("expected a fresh scrubbed doc for a different cache key (contextBlockID)")
	}

	// Different scope must isolate (no cross-scope reuse).
	cv5 := svc.cachedContextCore(context.Background(), "scope-2", provider, doc, nil, req)
	if cv5.scrubbedDoc == cv1.scrubbedDoc {
		t.Fatal("expected per-scope isolation: a different scope must not reuse another scope's clone")
	}
}

// TestInvalidateChatContext_BareStructLiteral verifies a ChatService built as
// a bare struct literal (chatCtxGen left as its zero value, as many tests and
// any not-yet-updated call site would do) still invalidates correctly instead
// of silently no-op'ing on a nil cache.
func TestInvalidateChatContext_BareStructLiteral(t *testing.T) {
	svc := &ChatService{chatCtxCache: newChatContextCache()}
	doc := toolLoopDoc()
	provider := ctxCacheStubProvider{}
	req := models.ChatRequest{Model: "m1", UserMessage: "turn 1"}

	cv1 := svc.cachedContextCore(context.Background(), "scope-1", provider, doc, nil, req)
	svc.InvalidateChatContext(doc.ID)
	cv2 := svc.cachedContextCore(context.Background(), "scope-1", provider, doc, nil, req)
	if cv2.scrubbedDoc == cv1.scrubbedDoc {
		t.Fatal("expected InvalidateChatContext to force a fresh scrubbed doc even without an explicit chatCtxGen")
	}
}

// TestChatCtxGen_BoundedAcrossManyFlows verifies the per-flow generation
// counter cache doesn't grow without bound: invalidating far more distinct
// flows than maxChatCtxGen evicts the oldest entries (LRU), so a long-lived
// process that edits many distinct flows over its uptime can't leak memory
// here the way an unbounded map would.
func TestChatCtxGen_BoundedAcrossManyFlows(t *testing.T) {
	svc := &ChatService{chatCtxCache: newChatContextCache()}
	genCache := svc.chatCtxGenCache()

	total := maxChatCtxGen * 4
	for i := range total {
		svc.InvalidateChatContext(fmt.Sprintf("flow-%d", i))
	}

	if _, ok := genCache.Get(context.Background(), "flow-0"); ok {
		t.Error("expected the earliest flow's generation entry to have been LRU-evicted")
	}
	last := fmt.Sprintf("flow-%d", total-1)
	if _, ok := genCache.Get(context.Background(), last); !ok {
		t.Errorf("expected the most recently invalidated flow (%s) to still be cached", last)
	}
}

// emitRecorder captures emitted (type, content) pairs for the coalescer tests.
type emitRecorder struct {
	mu    sync.Mutex
	calls []struct{ typ, content string }
}

func (r *emitRecorder) emit(typ string, data map[string]interface{}) {
	r.mu.Lock()
	defer r.mu.Unlock()
	content, _ := data["content"].(string)
	r.calls = append(r.calls, struct{ typ, content string }{typ, content})
}

// TestChunkCoalescer covers C-6: the first chunk emits immediately (first-token
// latency), subsequent deltas batch until flush, non-chunk events flush the
// batch before passing through, and the emitted-chunk count tracks emissions.
func TestChunkCoalescer(t *testing.T) {
	rec := &emitRecorder{}
	c := newChunkCoalescer(rec.emit)
	emit := c.wrap()

	// First chunk → immediate.
	emit("chunk", map[string]interface{}{"content": "Hel"})
	if len(rec.calls) != 1 || rec.calls[0].content != "Hel" {
		t.Fatalf("first chunk should emit immediately, got %+v", rec.calls)
	}

	// Subsequent chunks batch (no immediate emit).
	emit("chunk", map[string]interface{}{"content": "lo "})
	emit("chunk", map[string]interface{}{"content": "wor"})
	emit("chunk", map[string]interface{}{"content": "ld"})
	if len(rec.calls) != 1 {
		t.Fatalf("batched chunks should not emit until flush, got %d calls", len(rec.calls))
	}

	// A non-chunk event flushes the batch FIRST (in order), then passes through.
	emit("done", map[string]interface{}{"tokensIn": 10, "tokensOut": 5})
	if len(rec.calls) != 3 {
		t.Fatalf("expected flush+done = 3 calls, got %d (%+v)", len(rec.calls), rec.calls)
	}
	if rec.calls[1].typ != "chunk" || rec.calls[1].content != "lo world" {
		t.Errorf("batched chunk mismatch: got %+v", rec.calls[1])
	}
	if rec.calls[2].typ != "done" {
		t.Errorf("done should follow the flushed chunk, got %+v", rec.calls[2])
	}

	// Emitted-chunk count = 2 (first immediate + one merged batch).
	if n := c.flushAndCount(); n != 2 {
		t.Errorf("emitted chunk count = %d, want 2", n)
	}
}

// TestChunkCoalescer_EmptyChunkIgnored verifies a zero-content chunk (e.g. the
// scrubber emitting nothing for a partial token) doesn't pollute the batch.
func TestChunkCoalescer_EmptyChunkIgnored(t *testing.T) {
	rec := &emitRecorder{}
	c := newChunkCoalescer(rec.emit)
	emit := c.wrap()
	emit("chunk", map[string]interface{}{"content": "first"})  // immediate
	emit("chunk", map[string]interface{}{"content": ""})       // ignored
	emit("chunk", map[string]interface{}{"content": "second"}) // batched
	emit("done", map[string]interface{}{})                     // flush → "second"
	if len(rec.calls) != 3 {
		t.Fatalf("got %d calls, want 3 (first + second + done): %+v", len(rec.calls), rec.calls)
	}
	if rec.calls[1].content != "second" {
		t.Errorf("expected merged batch 'second', got %q", rec.calls[1].content)
	}
}

// nilSettings is a SettingsProvider whose Get returns nil → enforceBudget
// treats the day as unlimited. Used by stream tests that drive a successful
// (non-failing-key) provider path, which reaches enforceBudget.
type nilSettings struct{}

func (nilSettings) Get() *models.AppSettings          { return nil }
func (nilSettings) Update(models.AppSettings) error   { return nil }
func (nilSettings) AddRecentFile(string, int64) error { return nil }
func (nilSettings) RemoveRecentFile(string) error     { return nil }
func (nilSettings) ClearRecentFiles() error           { return nil }

// TestStreamChatMessage_PersistsUserTurnAtStart covers BUG-5: on the
// reconstruction path (client omitted Messages), the backend persists
// [history + new user turn] at stream start, so closing the app mid-stream
// (before any client save-on-done) retains the typed message. Uses the demo
// provider to drive a real successful stream end-to-end without network.
func TestStreamChatMessage_PersistsUserTurnAtStart(t *testing.T) {
	dir := t.TempDir()
	notifier := &testutil.CountingNotifier{}
	factory := ai.NewProviderFactory(func(_, _ string) (string, error) { return "k", nil }, nil, nil, nil)
	svc := &ChatService{
		notifier:      notifier,
		configDir:     dir,
		flowCache:     &FlowService{},
		analysisCache: &AnalysisService{},
		factory:       factory,
		settings:      nilSettings{},
		chatCtxCache:  newChatContextCache(),
	}
	doc := &models.FlowDocument{ID: "f1", Name: "f1"}

	// Pre-seed a prior conversation (the history to reconstruct).
	history := []models.ChatMessage{
		{ID: "h1", Role: "user", Content: "previous question"},
		{ID: "h2", Role: "assistant", Content: "previous answer"},
	}
	if err := svc.SaveConversation(context.Background(), doc, "flow", history); err != nil {
		t.Fatalf("seed conversation: %v", err)
	}

	// Stream with NO Messages (→ reconstruction path) and a fresh user turn.
	// No ClientStreamID, so the legacy begin-gated path runs; call BeginStream.
	id, err := svc.StreamChatMessage(context.Background(), "test", doc, nil, models.ChatRequest{
		Provider: "demo", Model: "demo", UserMessage: "fresh question",
	})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	svc.BeginStream(context.Background(), id)

	// Wait for the demo stream to finish.
	deadline := time.After(5 * time.Second)
	for {
		if _, ok := svc.activeStreams.Load(id); !ok {
			break
		}
		select {
		case <-deadline:
			t.Fatal("demo stream did not finish within 5s")
		case <-time.After(20 * time.Millisecond):
		}
	}

	// The store must hold [history + fresh user] — the start-persist retained
	// the user turn despite no client save-on-done running in this backend-only
	// test (mirrors a close-during-stream before onDone).
	got, err := svc.GetConversation(context.Background(), doc, "flow")
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 messages [history(2) + fresh user], got %d: %+v", len(got), got)
	}
	if got[2].Role != "user" || got[2].Content != "fresh question" {
		t.Errorf("last message = %+v, want role=user content='fresh question'", got[2])
	}
	// Prior history is preserved unchanged.
	if got[0].Content != "previous question" || got[1].Content != "previous answer" {
		t.Errorf("prior history altered: %+v", got[:2])
	}
}

// TestStreamChatMessage_LegacyMessagesDoesNotStartPersist verifies the
// start-persist is gated on the reconstruction path: when the client supplies
// Messages (legacy / resend override), the backend must NOT overwrite the store
// (the client owns persistence on that path).
func TestStreamChatMessage_LegacyMessagesDoesNotStartPersist(t *testing.T) {
	dir := t.TempDir()
	notifier := &testutil.CountingNotifier{}
	factory := ai.NewProviderFactory(func(_, _ string) (string, error) { return "k", nil }, nil, nil, nil)
	svc := &ChatService{
		notifier: notifier, configDir: dir, flowCache: &FlowService{},
		analysisCache: &AnalysisService{}, factory: factory, settings: nilSettings{}, chatCtxCache: newChatContextCache(),
	}
	doc := &models.FlowDocument{ID: "f1", Name: "f1"}

	// Pre-seed the store with a known history.
	if err := svc.SaveConversation(context.Background(), doc, "flow", []models.ChatMessage{
		{ID: "store1", Role: "user", Content: "stored"},
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Client supplies its OWN Messages (override path) — different from store.
	id, err := svc.StreamChatMessage(context.Background(), "test", doc, nil, models.ChatRequest{
		Provider: "demo", Model: "demo", UserMessage: "override",
		Messages: []models.ChatMessage{{ID: "c1", Role: "user", Content: "client-side history"}},
	})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	svc.BeginStream(context.Background(), id)
	// Let the worker reach/run. The start-persist must be SKIPPED (Messages non-empty).
	time.Sleep(150 * time.Millisecond)
	svc.CancelStream(id)

	// Store must be UNCHANGED (still the seeded single message) — the override
	// path did not backend-persist.
	got, err := svc.GetConversation(context.Background(), doc, "flow")
	if err != nil {
		t.Fatalf("GetConversation: %v", err)
	}
	if len(got) != 1 || got[0].Content != "stored" {
		t.Errorf("legacy/override path should not have backend-persisted; store = %+v", got)
	}
}
