package service

import (
	"context"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"

	"pad-analyzer/internal/ai"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// R-streaming-10: the watchdog's cancellations have wire codes of their own (STREAM_IDLE_TIMEOUT,
// STREAM_CLIENT_GONE), not the user's STREAM_STOPPED, and the idle message names the phase the stream stalled in, so
// a stall before the provider was ever called is not reported as the provider's.

// TestWatchStream_IdleMessageNamesThePhase: every phase's idle cancel carries STREAM_IDLE_TIMEOUT and copy naming
// the phase; only the provider phase blames the provider.
func TestWatchStream_IdleMessageNamesThePhase(t *testing.T) {
	for _, tc := range []struct {
		phase streamPhase
		want  string
	}{
		{phaseSetup, "setting up"},
		{phaseProvider, "the AI provider stopped responding"},
		{phaseTool, "tool"},
		{phaseApproval, "approval"},
		{phaseApply, "applying the fix"},
	} {
		t.Run(tc.phase.String(), func(t *testing.T) {
			svc := &ChatService{notifier: &testutil.CountingNotifier{}, watchdogInterval: 5 * time.Millisecond,
				idleTimeout: 30 * time.Millisecond, hostedIdleTimeout: 30 * time.Millisecond}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctl := &streamCtl{cancel: cancel, started: make(chan struct{})}
			ctl.touch()
			ctl.setPhase(tc.phase)
			go svc.watchStream(ctx, "s1", "user-1", ctl)
			select {
			case <-ctx.Done():
			case <-time.After(2 * time.Second):
				t.Fatal("the watchdog did not cancel an idle stream")
			}
			code, msg := ctl.classifyFailure(ctx, context.Canceled)
			if code != "STREAM_IDLE_TIMEOUT" || !strings.Contains(msg, tc.want) {
				t.Errorf("classifyFailure = %s %q, want STREAM_IDLE_TIMEOUT naming %q", code, msg, tc.want)
			}
			if tc.phase != phaseProvider && strings.Contains(msg, "provider stopped responding") {
				t.Errorf("a %s stall blames the provider: %q", tc.phase, msg)
			}
		})
	}
}

// stalledListingProvider never answers its model listing: a setup stall (the live Models() of the Copilot, GitHub
// Models or local providers) before any provider call.
type stalledListingProvider struct{ *testutil.FakeProvider }

func (p *stalledListingProvider) Models(ctx context.Context) ([]ai.ModelInfo, error) {
	<-ctx.Done()
	return nil, ctx.Err()
}

// The done-when: an idle cancel before the first provider call ends with a code other than STREAM_STOPPED and a
// message naming the setup phase; the log line names the phase too, and the turn's outcome stays idle_timeout.
func TestStream_SetupStallIsReportedAsSetup(t *testing.T) {
	sink := captureJSONLogs(t)
	p := &stalledListingProvider{FakeProvider: &testutil.FakeProvider{Fallback: &testutil.FakeTurn{Text: "never"}, HonourCtx: true}}
	svc := recordService(t, "watchdog-setup-stall", p)
	svc.watchdogInterval, svc.idleTimeout = 5*time.Millisecond, 40*time.Millisecond
	before := turnsCounted("idle_timeout")

	id := startRecordedTurn(t, svc, "watchdog-setup-stall")
	ctl := waitStreamFinished(t, svc, id)

	ctl.mu.Lock()
	code, msg := ctl.errCode, ctl.errMsg
	ctl.mu.Unlock()
	if code != "STREAM_IDLE_TIMEOUT" || !strings.Contains(msg, "setting up") || strings.Contains(msg, "provider stopped responding") {
		t.Errorf("the stream ended with %s %q, want STREAM_IDLE_TIMEOUT naming the setup phase", code, msg)
	}
	var phase any
	for _, l := range sink.lines() {
		if l["msg"] == "chat stream cancelled: idle" && l["stream_id"] == id {
			phase = l["phase"]
		}
	}
	if phase != "setup" {
		t.Errorf("idle log line phase = %v, want setup", phase)
	}
	if got := turnsCounted("idle_timeout"); got != before+1 {
		t.Errorf("idle_timeout turns counted %v, want %v", got, before+1)
	}
}

// slowSetupProvider takes d to build and d again for its first model listing: each setup step is under the idle
// limit, together they are over it.
type slowSetupProvider struct {
	*testutil.FakeProvider
	d      time.Duration
	listed atomic.Bool
}

func (p *slowSetupProvider) Models(ctx context.Context) ([]ai.ModelInfo, error) {
	if p.listed.CompareAndSwap(false, true) {
		select {
		case <-time.After(p.d):
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	return nil, nil
}

// The idle clock is touched at the setup boundaries (provider built, context built): a setup made of slow steps,
// each under the idle limit, is not cancelled for their sum.
func TestStream_SetupBoundariesTouchTheIdleClock(t *testing.T) {
	const step, idle = 250 * time.Millisecond, 400 * time.Millisecond
	p := &slowSetupProvider{FakeProvider: &testutil.FakeProvider{Fallback: &testutil.FakeTurn{Text: "the answer", TokensIn: 1, TokensOut: 1}, HonourCtx: true}, d: step}
	svc := recordService(t, "watchdog-slow-setup", p)
	var built atomic.Bool
	ai.RegisterProvider("watchdog-slow-setup", func(string) ai.Provider {
		if built.CompareAndSwap(false, true) {
			time.Sleep(step)
		}
		return p
	})
	svc.watchdogInterval, svc.idleTimeout = 5*time.Millisecond, idle

	req := models.ChatRequest{Provider: "watchdog-slow-setup", UserMessage: "q", ConversationID: uuid.NewString(),
		UserMessageID: uuid.NewString(), AssistantMessageID: uuid.NewString()}
	id, err := svc.StreamChatMessage(context.Background(), "test", &models.FlowDocument{ID: "f-record", Name: "f"}, nil, req)
	if err != nil {
		t.Fatal(err)
	}
	svc.BeginStream(context.Background(), id)
	ctl := waitStreamFinished(t, svc, id)
	ctl.mu.Lock()
	done, code, msg := ctl.done, ctl.errCode, ctl.errMsg
	ctl.mu.Unlock()
	if !built.Load() || !p.listed.Load() {
		t.Fatalf("the slow steps did not run (built %v, listed %v)", built.Load(), p.listed.Load())
	}
	if !done {
		t.Errorf("the stream ended with %s %q, want done (each setup step was under the idle limit)", code, msg)
	}
}

// An approval card's wait is the approval phase, and an approved apply the apply phase; the tool phase comes back
// when the apply is over.
func TestStream_ApprovalAndApplyPhases(t *testing.T) {
	t.Run("approval", func(t *testing.T) {
		doc := fixTestDoc()
		p := &testutil.FakeProvider{Tools: true, Turns: []testutil.FakeTurn{
			{ToolCalls: []ai.ToolCall{{ID: "t1", Name: "apply_fix", Input: []byte(`{"blockId":"b1","ruleId":"unhandled-error"}`)}}, TokensIn: 10, TokensOut: 5},
			{Text: "Fixed."},
		}}
		svc := failedTurnService(t, "watchdog-approval-phase", p, doc)
		req := models.ChatRequest{Provider: "watchdog-approval-phase", UserMessage: "fix it", UseTools: true, ConversationID: uuid.NewString(),
			UserMessageID: uuid.NewString(), AssistantMessageID: uuid.NewString()}
		id, err := svc.StreamChatMessage(context.Background(), "test", doc, nil, req)
		if err != nil {
			t.Fatal(err)
		}
		svc.BeginStream(context.Background(), id)
		var seen streamPhase = -1
		deadline := time.Now().Add(5 * time.Second)
		for seen < 0 && time.Now().Before(deadline) {
			svc.pendingFixDecisions.Range(func(k, v any) bool {
				if v.(*fixDecisionState).streamID != id {
					return true
				}
				if ctlV, ok := svc.activeStreams.Load(id); ok {
					seen = ctlV.(*streamCtl).currentPhase()
				}
				_ = svc.ResolveFixDecision(id, k.(string), true, nil)
				return false
			})
			time.Sleep(2 * time.Millisecond)
		}
		if ctl := waitStreamFinished(t, svc, id); !ctl.done {
			t.Fatalf("the turn failed: %s", ctl.errMsg)
		}
		if seen != phaseApproval {
			t.Errorf("phase while the card waited = %v, want %v", seen, phaseApproval)
		}
	})
	t.Run("apply", func(t *testing.T) {
		ctl := &streamCtl{cancel: func() {}}
		ctl.setPhase(phaseTool)
		release, ok := ctl.holdCancel()
		if !ok {
			t.Fatal("holdCancel refused on a live stream")
		}
		if got := ctl.currentPhase(); got != phaseApply {
			t.Errorf("phase while the apply holds = %v, want %v", got, phaseApply)
		}
		release()
		if got := ctl.currentPhase(); got != phaseTool {
			t.Errorf("phase after the apply = %v, want %v", got, phaseTool)
		}
	})
}

// The watchdog codes are turn failure codes: pad_chat_turns_total pre-initialises their outcome labels, which stay
// idle_timeout and subscriber_gone (step 8's), and the pre-initialised set has no duplicates.
func TestTurnOutcomes_WatchdogCodesKeepTheirLabels(t *testing.T) {
	codes := map[string]bool{}
	for _, c := range chatFailureCodes {
		codes[c] = true
	}
	for _, c := range []string{"STREAM_IDLE_TIMEOUT", "STREAM_CLIENT_GONE"} {
		if !codes[c] {
			t.Errorf("chatFailureCodes lacks %s", c)
		}
	}
	labels := map[string]int{}
	for _, l := range chatTurnOutcomes() {
		labels[l]++
	}
	for l, n := range labels {
		if n > 1 {
			t.Errorf("outcome label %q pre-initialised %d times", l, n)
		}
	}
	for _, l := range []string{"done", "error", "stopped", "idle_timeout", "subscriber_gone"} {
		if labels[l] == 0 {
			t.Errorf("outcome label %q is not pre-initialised", l)
		}
	}
	for _, l := range []string{"stream_idle_timeout", "stream_client_gone"} {
		if labels[l] != 0 {
			t.Errorf("outcome label %q: the watchdog codes keep the step-8 labels", l)
		}
	}
}

// A batch's fixes not yet written when a stop arrived are answered with why: the watchdog's two stops are not the
// user's ("the response was stopped"), nor a restart.
func TestStopBetweenFixes_WordsTheWatchdogStops(t *testing.T) {
	for _, tc := range []struct{ code, want string }{
		{"", "the response was stopped"},
		{ChatErrServerRestarting, "the server is restarting"},
		{ChatErrIdleTimeout, "stopped making progress"},
		{ChatErrClientGone, "you were disconnected"},
	} {
		t.Run(cmpOr(tc.code, "user stop"), func(t *testing.T) {
			svc := &ChatService{}
			ctl := &streamCtl{cancel: func() {}}
			svc.activeStreams.Store("s1", ctl)
			release, ok := ctl.holdCancel()
			if !ok {
				t.Fatal("holdCancel refused")
			}
			defer release()
			ctl.cancelWithCode(tc.code, "stopped")
			if got := svc.stopBetweenFixes("s1")(); !strings.HasPrefix(got, "not attempted: ") || !strings.Contains(got, tc.want) {
				t.Errorf("stopBetweenFixes = %q, want it to say %q", got, tc.want)
			}
		})
	}
}

func cmpOr(s, fallback string) string {
	if s == "" {
		return fallback
	}
	return s
}
