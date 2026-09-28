package service

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// silentHostedProvider streams what a Foundry agent turn with a slow hosted
// tool looks like: the step starts, the provider stays silent while the tool
// runs server-side, then the step ends and the answer follows.
type silentHostedProvider struct {
	*testutil.FakeProvider
	silence   time.Duration
	openStep  bool // emit the hosted-step start before the silence
	leaveOpen bool // first turn: never finish the step, request a local tool instead
	// preText emits an answer chunk BEFORE the silence: the silence is then
	// judged at the normal idle limit (silence before a provider's first
	// output gets the hosted limit — a reasoning model thinking silently).
	preText bool
	calls   atomic.Int32
}

func (p *silentHostedProvider) SupportsTools() bool { return true }

func (p *silentHostedProvider) Stream(ctx context.Context, _ ai.Request, onChunk func(ai.Chunk)) error {
	turn := p.calls.Add(1)
	if p.leaveOpen && turn == 1 {
		// A hosted step whose completion never arrives, then a function
		// call: the tool loop runs the tool and starts a second turn.
		onChunk(ai.Chunk{ToolEvent: &ai.ToolEvent{Name: "mcp_call", Label: "Agent · MCP tool"}})
		onChunk(ai.Chunk{Done: true, TokensIn: 1, TokensOut: 1, ToolCalls: []ai.ToolCall{{ID: "c1", Name: "search_flow", Input: json.RawMessage(`{"query":"x"}`)}}})
		return nil
	}
	if p.openStep {
		onChunk(ai.Chunk{ToolEvent: &ai.ToolEvent{Name: "mcp_call", Label: "Agent · MCP tool"}})
	}
	if p.preText {
		onChunk(ai.Chunk{Text: "first, "})
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(p.silence):
	}
	if p.openStep {
		onChunk(ai.Chunk{ToolEvent: &ai.ToolEvent{Name: "mcp_call", Label: "Agent · MCP tool", Done: true, OK: true}})
	}
	onChunk(ai.Chunk{Text: "answer"})
	onChunk(ai.Chunk{Done: true, TokensIn: 3, TokensOut: 2})
	return nil
}

// testIdleLimit is the idle limit the tests run with: generous against the
// warm pre-stream latency (~10 ms), small against the silences below.
const testIdleLimit = 200 * time.Millisecond

var warmChatPath sync.Once

// runSilentHostedStream runs one chat stream against p with testIdleLimit
// and returns the terminal event type and its payload.
func runSilentHostedStream(t *testing.T, p *silentHostedProvider, useTools bool, hostedIdle time.Duration) (string, map[string]interface{}) {
	t.Helper()
	// The first stream in a process spends ~100 ms on one-time setup before
	// the provider is called; absorb it with the default idle limit.
	warmChatPath.Do(func() {
		warm := &silentHostedProvider{FakeProvider: &testutil.FakeProvider{}}
		if typ, data := runChatStream(t, warm, false, 0, 0); typ != "done" {
			t.Fatalf("warm-up stream ended with %s %v", typ, data)
		}
	})
	return runChatStream(t, p, useTools, testIdleLimit, hostedIdle)
}

func runChatStream(t *testing.T, p *silentHostedProvider, useTools bool, idle, hostedIdle time.Duration) (string, map[string]interface{}) {
	t.Helper()
	ai.RegisterProvider("hosted-idle-fake", func(string) ai.Provider { return p })
	notifier := &testutil.CountingNotifier{}
	factory := ai.NewProviderFactory(func(_, _ string) (string, error) { return "k", nil }, nil, nil, nil)
	svc := &ChatService{
		notifier:          notifier,
		configDir:         t.TempDir(),
		flowCache:         &FlowService{},
		analysisCache:     &AnalysisService{},
		factory:           factory,
		settings:          nilSettings{},
		chatCtxCache:      newChatContextCache(),
		watchdogInterval:  10 * time.Millisecond,
		idleTimeout:       idle,
		hostedIdleTimeout: hostedIdle,
	}
	doc := &models.FlowDocument{ID: "f1", Name: "f1"}
	id, err := svc.StreamChatMessage(context.Background(), "test", doc, nil, models.ChatRequest{
		Provider: "hosted-idle-fake", Model: "gpt-4o", UserMessage: "run the slow tool", UseTools: useTools,
	})
	if err != nil {
		t.Fatalf("StreamChatMessage: %v", err)
	}
	svc.BeginStream(context.Background(), id)
	// The stream's recorded terminal state is authoritative: a failure that
	// precedes any output may reach the client through resume rather than a
	// live event.
	deadline := time.After(5 * time.Second)
	for {
		if v, ok := svc.finishedStreams.Load(id); ok {
			ctl := v.(*streamCtl)
			select { // settled: the detached answer persist has landed (D-4)
			case <-ctl.settled:
			case <-deadline:
				t.Fatal("stream did not settle within 5s")
			}
			snap := ctl.snapshot()
			switch {
			case snap.Error != "":
				return "error", map[string]interface{}{"message": snap.Error, "code": snap.ErrorCode}
			case snap.Done:
				return "done", nil
			}
			types, _ := eventTypes(notifier)
			t.Fatalf("stream finished without a terminal state: %v", types)
		}
		select {
		case <-deadline:
			t.Fatal("stream did not finish within 5s")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

const providerStalled = "the AI provider stopped responding"

// TestStream_OpenHostedStepOutlastsTheIdleLimit: a hosted step that runs
// silently for 4× the idle limit is not killed by the watchdog (the idle
// limit is hostedStepIdleTimeout while it is open), on both stream paths.
func TestStream_OpenHostedStepOutlastsTheIdleLimit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		useTools bool
	}{{"plain stream path", false}, {"tool loop path", true}} {
		t.Run(tc.name, func(t *testing.T) {
			p := &silentHostedProvider{FakeProvider: &testutil.FakeProvider{}, silence: 4 * testIdleLimit, openStep: true}
			typ, data := runSilentHostedStream(t, p, tc.useTools, 0)
			if typ != "done" {
				t.Errorf("stream ended with %s %v, want done", typ, data)
			}
		})
	}
}

// TestStream_SilenceWithoutHostedStepStillIdleCancelled: the same silence
// with no hosted step open, once the provider has produced output, still
// trips the normal idle limit.
func TestStream_SilenceWithoutHostedStepStillIdleCancelled(t *testing.T) {
	for _, tc := range []struct {
		name     string
		useTools bool
	}{{"plain stream path", false}, {"tool loop path", true}} {
		t.Run(tc.name, func(t *testing.T) {
			p := &silentHostedProvider{FakeProvider: &testutil.FakeProvider{}, silence: 2 * time.Second, preText: true}
			typ, data := runSilentHostedStream(t, p, tc.useTools, 0)
			if msg, _ := data["message"].(string); typ != "error" || !strings.Contains(msg, providerStalled) {
				t.Errorf("stream ended with %s %v, want the provider-stalled error", typ, data)
			}
		})
	}
}

// PERF-3: a provider that streams nothing before its first output — a
// reasoning deployment thinking, a Responses run whose argument deltas the
// parser does not surface — is given the hosted-step limit, not the 90 s
// idle limit, on both stream paths.
func TestStream_SilenceBeforeFirstOutputUsesHostedLimit(t *testing.T) {
	for _, tc := range []struct {
		name     string
		useTools bool
	}{{"plain stream path", false}, {"tool loop path", true}} {
		t.Run(tc.name, func(t *testing.T) {
			p := &silentHostedProvider{FakeProvider: &testutil.FakeProvider{}, silence: 4 * testIdleLimit}
			typ, data := runSilentHostedStream(t, p, tc.useTools, 0)
			if typ != "done" {
				t.Errorf("stream ended with %s %v, want done (silence before the first output is thinking, not a hang)", typ, data)
			}
		})
	}
	// Still a bound: a provider that never produces anything is caught at
	// the hosted limit.
	t.Run("hung before first output is still bounded", func(t *testing.T) {
		p := &silentHostedProvider{FakeProvider: &testutil.FakeProvider{}, silence: 3 * time.Second}
		started := time.Now()
		typ, data := runSilentHostedStream(t, p, false, 2*testIdleLimit)
		if msg, _ := data["message"].(string); typ != "error" || !strings.Contains(msg, providerStalled) {
			t.Errorf("stream ended with %s %v, want the provider-stalled error", typ, data)
		}
		if took := time.Since(started); took > 2500*time.Millisecond {
			t.Errorf("hung provider cancelled after %v, want about the hosted limit", took)
		}
	})
}

// TestStream_HungHostedStepStillBounded: the raised limit is a bound, not a
// suspension of hang detection.
func TestStream_HungHostedStepStillBounded(t *testing.T) {
	p := &silentHostedProvider{FakeProvider: &testutil.FakeProvider{}, silence: 3 * time.Second, openStep: true}
	started := time.Now()
	typ, data := runSilentHostedStream(t, p, true, 2*testIdleLimit)
	if msg, _ := data["message"].(string); typ != "error" || !strings.Contains(msg, providerStalled) {
		t.Errorf("stream ended with %s %v, want the provider-stalled error", typ, data)
	}
	if took := time.Since(started); took > 2500*time.Millisecond {
		t.Errorf("hung hosted step cancelled after %v, want about the hosted limit", took)
	}
}

// TestStream_HostedStepCountResetAfterProviderTurn: a step whose completion
// never arrived must not keep the raised limit for the next provider turn
// (which answers first, then goes silent — so only a leaked hosted-step
// count could keep it alive).
func TestStream_HostedStepCountResetAfterProviderTurn(t *testing.T) {
	p := &silentHostedProvider{FakeProvider: &testutil.FakeProvider{}, silence: 2 * time.Second, leaveOpen: true, preText: true}
	typ, data := runSilentHostedStream(t, p, true, 0)
	if msg, _ := data["message"].(string); typ != "error" || !strings.Contains(msg, providerStalled) {
		t.Errorf("stream ended with %s %v, want the provider-stalled error in the second turn", typ, data)
	}
	if got := p.calls.Load(); got != 2 {
		t.Errorf("provider turns = %d, want 2", got)
	}
}

func TestStreamCtl_HostedStepNeverNegative(t *testing.T) {
	ctl := &streamCtl{}
	ctl.hostedStep(false) // a completion whose start was not seen
	if n := ctl.hostedOpen.Load(); n != 0 {
		t.Fatalf("count after unmatched completion = %d", n)
	}
	ctl.hostedStep(true)
	ctl.hostedStep(true)
	ctl.hostedStep(false)
	if n := ctl.hostedOpen.Load(); n != 1 {
		t.Fatalf("count = %d, want 1", n)
	}
	svc := &ChatService{idleTimeout: 50 * time.Millisecond}
	if got := svc.idleLimitFor(ctl); got != hostedStepIdleTimeout {
		t.Errorf("idle limit with an open step = %v, want %v", got, hostedStepIdleTimeout)
	}
	if hostedStepIdleTimeout >= maxChatStreamDuration {
		t.Errorf("hosted idle limit %v must stay under the stream cap %v", hostedStepIdleTimeout, maxChatStreamDuration)
	}
	ctl.endProviderTurn()
	if got := svc.idleLimitFor(ctl); got != 50*time.Millisecond {
		t.Errorf("idle limit after the turn = %v, want the normal limit", got)
	}
	// PERF-3: awaiting the first output raises the limit the same way; the
	// first text / tool-call / terminal chunk, or the turn's end, lowers it.
	ctl.beginProviderTurn()
	if got := svc.idleLimitFor(ctl); got != hostedStepIdleTimeout {
		t.Errorf("idle limit while awaiting output = %v, want %v", got, hostedStepIdleTimeout)
	}
	ctl.sawOutput(false, false, false) // a keepalive proves nothing
	if got := svc.idleLimitFor(ctl); got != hostedStepIdleTimeout {
		t.Errorf("a keepalive must not clear the awaiting-output limit, got %v", got)
	}
	ctl.sawOutput(true, false, false)
	if got := svc.idleLimitFor(ctl); got != 50*time.Millisecond {
		t.Errorf("idle limit after the first output = %v, want the normal limit", got)
	}
	ctl.beginProviderTurn()
	ctl.endProviderTurn()
	if got := svc.idleLimitFor(ctl); got != 50*time.Millisecond {
		t.Errorf("idle limit after a turn that produced nothing = %v, want the normal limit", got)
	}
}
