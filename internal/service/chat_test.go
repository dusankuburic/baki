package service

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/models"
	"pad-analyzer/internal/testutil"
)

// TestStreamChatMessage_CancelBeforeBegin_ReleasesGoroutine verifies that a
// stream which the client never starts (no /api/chat/begin) is released when
// it is explicitly cancelled, instead of blocking on `<-ctl.started` until the
// 5-minute upper-bound timeout.
func TestStreamChatMessage_CancelBeforeBegin_ReleasesGoroutine(t *testing.T) {
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
		notifier: notifier,
		flowCache:     &FlowService{},
		analysisCache: &AnalysisService{},
		factory:  factory,
	}

	id, err := svc.StreamChatMessage(context.Background(), "test", nil, nil, models.ChatRequest{Provider: "unknown"})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}

	svc.CancelStream(id)

	deadline := time.After(2 * time.Second)
	for {
		if _, ok := svc.activeStreams.Load(id); !ok {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("stream goroutine did not exit within 2s after CancelStream")
		case <-time.After(10 * time.Millisecond):
		}
	}

	if got := notifier.Count(); got != 0 {
		t.Errorf("expected 0 events emitted for cancelled-before-begin stream, got %d", got)
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

	svc.BeginStream(id)

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
		notifier: notifier,
		flowCache:     &FlowService{},
		analysisCache: &AnalysisService{},
		factory:  factory,
	}

	id, err := svc.StreamChatMessage(context.Background(), "test", nil, nil, models.ChatRequest{Provider: "unknown"})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}

	svc.BeginStream(id)

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
		func(scope, provider string) (string, error) {return "", fmt.Errorf("no key")},
		nil,
		nil,
		nil,
	)
	svc := &ChatService{
		notifier: notifier,
		flowCache:     &FlowService{},
		analysisCache: &AnalysisService{},
		factory:  factory,
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
			svc.BeginStream(id)
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

func TestNormalizeChatParams(t *testing.T) {
	cases := []struct {
		name      string
		temp      float64
		maxTok    int
		ctxLimit  int
		maxOutput int
		wantTemp  float64
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

// loopTurn scripts one streamed turn: optional assistant text streamed first,
// then a terminal chunk carrying any tool calls and token usage.
type loopTurn struct {
	text      string
	toolCalls []ai.ToolCall
	tokensIn  int
	tokensOut int
}

// loopStub scripts a sequence of streamed turns; only Stream is exercised by
// runToolLoop (other Provider methods are never called).
type loopStub struct {
	ai.Provider
	calls int
	turns []loopTurn
	err   error
}

func (s *loopStub) Stream(_ context.Context, _ ai.Request, onChunk func(ai.Chunk)) error {
	if s.err != nil {
		return s.err
	}
	i := s.calls
	s.calls++
	var turn loopTurn
	if i < len(s.turns) {
		turn = s.turns[i]
	} else {
		// Default: keep requesting a tool (drives the iteration-cap path).
		turn = loopTurn{toolCalls: []ai.ToolCall{{ID: "t", Name: "search_flow", Input: []byte(`{"query":"x"}`)}}}
	}
	if turn.text != "" {
		onChunk(ai.Chunk{Text: turn.text})
	}
	onChunk(ai.Chunk{Done: true, ToolCalls: turn.toolCalls, TokensIn: turn.tokensIn, TokensOut: turn.tokensOut})
	return nil
}

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
	if usage := svc.dailyUsage(context.Background(), "user-1", "org-1"); usage != 0 {
		t.Errorf("dailyUsage with nil backend = %v, want 0", usage)
	}
}

func TestRunToolLoop_ExecutesToolThenFinal(t *testing.T) {
	stub := &loopStub{turns: []loopTurn{
		{toolCalls: []ai.ToolCall{{ID: "t1", Name: "search_flow", Input: []byte(`{"query":"xav"}`)}}, tokensIn: 10, tokensOut: 5},
		{text: "Final answer", tokensIn: 8, tokensOut: 3},
	}}
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := &streamCtl{}
	emit, evs := collectEvents()

	svc.runToolLoop(context.Background(), stub, ai.Request{Messages: []ai.Message{{Role: "user", Content: "hi"}}},
		toolLoopDoc(), ctl, func() bool { return true }, emit)

	if stub.calls != 2 {
		t.Fatalf("expected 2 Stream calls, got %d", stub.calls)
	}
	if got := strings.Join(*evs, ","); got != "tool,chunk,done" {
		t.Errorf("expected tool,chunk,done events, got %q", got)
	}
	if !ctl.done || ctl.buffer.String() != "Final answer" {
		t.Errorf("ctl not finalized: done=%v buffer=%q", ctl.done, ctl.buffer.String())
	}
	if ctl.tokensIn != 18 || ctl.tokensOut != 8 {
		t.Errorf("tokens not summed across turns: in=%d out=%d", ctl.tokensIn, ctl.tokensOut)
	}
}

func TestRunToolLoop_NoToolsFinalImmediately(t *testing.T) {
	stub := &loopStub{turns: []loopTurn{{text: "Direct", tokensIn: 1, tokensOut: 2}}}
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := &streamCtl{}
	emit, evs := collectEvents()

	svc.runToolLoop(context.Background(), stub, ai.Request{}, toolLoopDoc(), ctl, func() bool { return true }, emit)

	if stub.calls != 1 {
		t.Fatalf("expected 1 Stream call, got %d", stub.calls)
	}
	if got := strings.Join(*evs, ","); got != "chunk,done" {
		t.Errorf("expected chunk,done, got %q", got)
	}
}

func TestRunToolLoop_ChatErrorEmitsError(t *testing.T) {
	stub := &loopStub{err: fmt.Errorf("boom")}
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := &streamCtl{}
	emit, evs := collectEvents()

	svc.runToolLoop(context.Background(), stub, ai.Request{}, toolLoopDoc(), ctl, func() bool { return true }, emit)

	if got := strings.Join(*evs, ","); got != "error" {
		t.Errorf("expected single error event, got %q", got)
	}
	if ctl.errMsg == "" {
		t.Error("expected ctl.errMsg set")
	}
}

func TestRunToolLoop_IterationCap(t *testing.T) {
	stub := &loopStub{} // always returns a tool call → never terminates
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := &streamCtl{}
	emit, evs := collectEvents()

	svc.runToolLoop(context.Background(), stub, ai.Request{}, toolLoopDoc(), ctl, func() bool { return true }, emit)

	if stub.calls != maxToolIterations {
		t.Errorf("expected %d Stream calls at cap, got %d", maxToolIterations, stub.calls)
	}
	last := (*evs)[len(*evs)-1]
	if last != "error" {
		t.Errorf("expected final error event at iteration cap, got %q", last)
	}
}

func TestRunToolLoop_NotStartedEmitsNothing(t *testing.T) {
	stub := &loopStub{turns: []loopTurn{{text: "x"}}}
	svc := &ChatService{analysisCache: &AnalysisService{}}
	ctl := &streamCtl{}
	emit, evs := collectEvents()

	// awaitStart returns false (client cancelled before begin) → no events.
	svc.runToolLoop(context.Background(), stub, ai.Request{}, toolLoopDoc(), ctl, func() bool { return false }, emit)

	if len(*evs) != 0 {
		t.Errorf("expected no events when not started, got %v", *evs)
	}
}
