package ai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// retryStub lets a test script a sequence of Chat/Stream outcomes.
type retryStub struct {
	Provider
	chatErrs   []error // returned in order, one per call
	chatCalls  int
	streamPlan []func(onChunk func(Chunk)) error
	streamCall int
}

func (s *retryStub) Chat(_ context.Context, _ Request) (*Response, error) {
	i := s.chatCalls
	s.chatCalls++
	if i < len(s.chatErrs) {
		if s.chatErrs[i] != nil {
			return nil, s.chatErrs[i]
		}
	}
	return &Response{Content: "ok"}, nil
}

func (s *retryStub) Stream(_ context.Context, _ Request, onChunk func(Chunk)) error {
	i := s.streamCall
	s.streamCall++
	if i < len(s.streamPlan) {
		return s.streamPlan[i](onChunk)
	}
	return nil
}

func TestRetry_Chat_RetriesOnRateLimitThenSucceeds(t *testing.T) {
	stub := &retryStub{chatErrs: []error{ErrRateLimited, ErrRateLimited, nil}}
	rp := NewRetryingProvider(stub)

	resp, err := rp.Chat(context.Background(), Request{})
	if err != nil {
		t.Fatalf("expected success after retries, got %v", err)
	}
	if resp.Content != "ok" {
		t.Errorf("unexpected content %q", resp.Content)
	}
	if stub.chatCalls != 3 {
		t.Errorf("expected 3 Chat calls (2 retries), got %d", stub.chatCalls)
	}
}

func TestRetry_Chat_DoesNotRetryPermanentError(t *testing.T) {
	stub := &retryStub{chatErrs: []error{ErrApiKeyInvalid}}
	rp := NewRetryingProvider(stub)

	_, err := rp.Chat(context.Background(), Request{})
	if !errors.Is(err, ErrApiKeyInvalid) {
		t.Fatalf("expected ErrApiKeyInvalid, got %v", err)
	}
	if stub.chatCalls != 1 {
		t.Errorf("expected exactly 1 Chat call for permanent error, got %d", stub.chatCalls)
	}
}

func TestRetry_Chat_StopsAtMaxAttempts(t *testing.T) {
	stub := &retryStub{chatErrs: []error{ErrRateLimited, ErrRateLimited, ErrRateLimited, ErrRateLimited}}
	rp := NewRetryingProvider(stub)

	_, err := rp.Chat(context.Background(), Request{})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited after exhausting retries, got %v", err)
	}
	if stub.chatCalls != retryMaxAttempts {
		t.Errorf("expected %d Chat calls, got %d", retryMaxAttempts, stub.chatCalls)
	}
}

func TestRetry_Stream_NoRetryAfterChunkEmitted(t *testing.T) {
	// First attempt emits a chunk THEN fails — must NOT retry (can't replay).
	stub := &retryStub{streamPlan: []func(onChunk func(Chunk)) error{
		func(onChunk func(Chunk)) error {
			onChunk(Chunk{Text: "partial"})
			return ErrRateLimited
		},
	}}
	rp := NewRetryingProvider(stub)

	err := rp.Stream(context.Background(), Request{}, func(Chunk) {})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("expected ErrRateLimited returned, got %v", err)
	}
	if stub.streamCall != 1 {
		t.Errorf("expected exactly 1 Stream call (no replay after partial output), got %d", stub.streamCall)
	}
}

func TestRetry_Stream_RetriesWhenFailedBeforeAnyChunk(t *testing.T) {
	stub := &retryStub{streamPlan: []func(onChunk func(Chunk)) error{
		func(func(Chunk)) error { return ErrRateLimited }, // fails before emitting
		func(onChunk func(Chunk)) error { onChunk(Chunk{Text: "ok", Done: true}); return nil },
	}}
	rp := NewRetryingProvider(stub)

	var got string
	err := rp.Stream(context.Background(), Request{}, func(c Chunk) { got += c.Text })
	if err != nil {
		t.Fatalf("expected success on retry, got %v", err)
	}
	if got != "ok" {
		t.Errorf("expected streamed content 'ok', got %q", got)
	}
	if stub.streamCall != 2 {
		t.Errorf("expected 2 Stream calls (1 retry), got %d", stub.streamCall)
	}
}

// TestRetry_Stream_RetriesErrorChunkBeforeContent covers the mid-SSE failure
// mode: the parser surfaces an upstream error event as Chunk{Error} and
// returns nil. Before any content has been delivered that is equivalent to a
// pre-stream failure, so it must be held back from the caller and retried.
func TestRetry_Stream_RetriesErrorChunkBeforeContent(t *testing.T) {
	stub := &retryStub{streamPlan: []func(onChunk func(Chunk)) error{
		func(onChunk func(Chunk)) error {
			onChunk(Chunk{TokensIn: 5}) // metadata only — must not count as content
			onChunk(Chunk{Error: fmt.Errorf("%w: overloaded", ErrProviderDown)})
			return nil
		},
		func(onChunk func(Chunk)) error {
			onChunk(Chunk{Text: "ok"})
			onChunk(Chunk{Done: true})
			return nil
		},
	}}
	rp := NewRetryingProvider(stub)

	var got string
	errChunks := 0
	err := rp.Stream(context.Background(), Request{}, func(c Chunk) {
		got += c.Text
		if c.Error != nil {
			errChunks++
		}
	})
	if err != nil {
		t.Fatalf("expected success on retry, got %v", err)
	}
	if got != "ok" {
		t.Errorf("expected streamed content 'ok', got %q", got)
	}
	if errChunks != 0 {
		t.Errorf("held-back error chunk leaked to the caller %d time(s)", errChunks)
	}
	if stub.streamCall != 2 {
		t.Errorf("expected 2 Stream calls (1 retry), got %d", stub.streamCall)
	}
}

// TestRetry_Stream_ErrorChunkAfterContentForwardedNotRetried: once text has
// been delivered the partial stream cannot be replayed — the error chunk must
// pass through to the caller unchanged and the attempt must not repeat.
func TestRetry_Stream_ErrorChunkAfterContentForwardedNotRetried(t *testing.T) {
	stub := &retryStub{streamPlan: []func(onChunk func(Chunk)) error{
		func(onChunk func(Chunk)) error {
			onChunk(Chunk{Text: "partial"})
			onChunk(Chunk{Error: fmt.Errorf("%w: overloaded", ErrProviderDown)})
			return nil
		},
	}}
	rp := NewRetryingProvider(stub)

	errChunks := 0
	err := rp.Stream(context.Background(), Request{}, func(c Chunk) {
		if c.Error != nil {
			errChunks++
		}
	})
	if err != nil {
		t.Fatalf("expected nil return (error travels as chunk after content), got %v", err)
	}
	if errChunks != 1 {
		t.Errorf("expected the post-content error chunk delivered once, got %d", errChunks)
	}
	if stub.streamCall != 1 {
		t.Errorf("expected exactly 1 Stream call (no replay after partial output), got %d", stub.streamCall)
	}
}

// TestRetry_Stream_HostedStepCountsAsDelivered: once the provider ran a
// server-side step — shown in the trail (ToolEvent) or not (ServerStep) — a
// replay would run it again, so a later retryable error is forwarded instead.
func TestRetry_Stream_HostedStepCountsAsDelivered(t *testing.T) {
	for _, tc := range []struct {
		name string
		step Chunk
	}{
		{"trail ToolEvent", Chunk{ToolEvent: &ToolEvent{Name: "web_search_call", Label: "Agent · web search"}}},
		{"unlabelled ServerStep", Chunk{ServerStep: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &retryStub{streamPlan: []func(onChunk func(Chunk)) error{
				func(onChunk func(Chunk)) error {
					onChunk(tc.step)
					onChunk(Chunk{Error: fmt.Errorf("%w: slow down", ErrRateLimited)})
					return nil
				},
				func(onChunk func(Chunk)) error {
					onChunk(Chunk{Text: "replayed"})
					onChunk(Chunk{Done: true})
					return nil
				},
			}}
			rp := NewRetryingProviderWithConfig(stub, 3, time.Millisecond)
			var errs []error
			err := rp.Stream(context.Background(), Request{}, func(c Chunk) {
				if c.Error != nil {
					errs = append(errs, c.Error)
				}
			})
			if err != nil {
				t.Fatalf("expected nil return (the error travels as a chunk after the step), got %v", err)
			}
			if stub.streamCall != 1 {
				t.Errorf("expected exactly 1 Stream call (no replay after a server-side step), got %d", stub.streamCall)
			}
			if len(errs) != 1 || !errors.Is(errs[0], ErrRateLimited) {
				t.Errorf("forwarded errors = %v", errs)
			}
		})
	}
}

// TestRetry_Stream_PermanentErrorChunkReturnedNotRetried: a pre-content error
// chunk that is not retryable is converted into a returned error (so callers
// and outer wrappers see a real failure) without burning retry attempts.
func TestRetry_Stream_PermanentErrorChunkReturnedNotRetried(t *testing.T) {
	stub := &retryStub{streamPlan: []func(onChunk func(Chunk)) error{
		func(onChunk func(Chunk)) error {
			onChunk(Chunk{Error: errors.New("invalid request")})
			return nil
		},
	}}
	rp := NewRetryingProvider(stub)

	err := rp.Stream(context.Background(), Request{}, func(Chunk) {})
	if err == nil || err.Error() != "invalid request" {
		t.Fatalf("expected the chunk error returned, got %v", err)
	}
	if stub.streamCall != 1 {
		t.Errorf("expected exactly 1 Stream call for permanent error, got %d", stub.streamCall)
	}
}

// TestRetry_Stream_HeldErrorChunkKeepsUsage: an error chunk withheld before
// delivery may be the only place a failed run's token usage rides (the
// MCP-approvals error is one such); the error Stream returns must carry it so
// the audited wrapper can bill the run instead of estimating.
func TestRetry_Stream_HeldErrorChunkKeepsUsage(t *testing.T) {
	stub := &retryStub{streamPlan: []func(onChunk func(Chunk)) error{
		func(onChunk func(Chunk)) error {
			// No content, no Done: the error chunk is the whole run.
			onChunk(Chunk{Error: errors.New("the agent asked for approval"), TokensIn: 11, TokensOut: 4, ServedModel: "gpt-4.1"})
			return nil
		},
	}}
	rp := NewRetryingProvider(stub)

	err := rp.Stream(context.Background(), Request{}, func(Chunk) {})
	if err == nil || err.Error() != "the agent asked for approval" {
		t.Fatalf("expected the chunk error returned unchanged, got %v", err)
	}
	in, out, served, ok := usageOf(err)
	if !ok || in != 11 || out != 4 || served != "gpt-4.1" {
		t.Errorf("usage on the returned error = %d/%d/%q (ok=%v), want 11/4/\"gpt-4.1\"", in, out, served, ok)
	}
	if stub.streamCall != 1 {
		t.Errorf("a plain error must not be retried, got %d calls", stub.streamCall)
	}
}

// TestAudited_StreamBillsUsageCarriedOnHeldError: end to end through the real
// wrapper chain (audited outside retry), a failed run whose only usage rode
// the withheld error chunk records those exact counts, not an estimate.
func TestAudited_StreamBillsUsageCarriedOnHeldError(t *testing.T) {
	stub := &auditStub{
		models:       []ModelInfo{{ID: "m", Pricing: Pricing{InputCostPerM: 1, OutputCostPerM: 1}}},
		streamChunks: []Chunk{{Error: errors.New("the agent asked for approval"), TokensIn: 11, TokensOut: 4, ServedModel: "gpt-4.1"}},
	}
	recorded := make(chan *interfaces.UsageMetric, 1)
	ap := NewAuditedProvider(NewRetryingProviderWithConfig(stub, 3, time.Millisecond),
		func(_ context.Context, m *interfaces.UsageMetric) error { recorded <- m; return nil },
		"user-1", "stub")
	if err := ap.Stream(context.Background(), Request{Model: "m"}, func(Chunk) {}); err == nil {
		t.Fatal("expected the error chunk to surface as the Stream error")
	}
	select {
	case m := <-recorded:
		if m.PromptTokens != 11 || m.CompletionTokens != 4 {
			t.Errorf("recorded %d/%d tokens, want the 11/4 carried on the withheld error chunk", m.PromptTokens, m.CompletionTokens)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no usage recorded for the failed run")
	}
}

func TestRateLimitError_UnwrapsAndIsRetryable(t *testing.T) {
	err := &RateLimitError{RetryAfter: 2 * time.Second}
	if !errors.Is(err, ErrRateLimited) {
		t.Fatal("RateLimitError should unwrap to ErrRateLimited")
	}
	if !isRetryable(err) {
		t.Fatal("RateLimitError should be retryable")
	}
}

func TestParseRetryAfter(t *testing.T) {
	mk := func(v string) *http.Response {
		h := http.Header{}
		if v != "" {
			h.Set("Retry-After", v)
		}
		return &http.Response{Header: h}
	}
	if got := parseRetryAfter(mk("2")); got != 2*time.Second {
		t.Errorf("delta-seconds: got %v, want 2s", got)
	}
	if got := parseRetryAfter(mk("")); got != 0 {
		t.Errorf("missing header: got %v, want 0", got)
	}
	if got := parseRetryAfter(mk("garbage")); got != 0 {
		t.Errorf("invalid header: got %v, want 0", got)
	}
	if got := parseRetryAfter(mk("-5")); got != 0 {
		t.Errorf("negative seconds: got %v, want 0", got)
	}
	future := time.Now().Add(30 * time.Second).UTC().Format(http.TimeFormat)
	if got := parseRetryAfter(mk(future)); got <= 0 || got > 31*time.Second {
		t.Errorf("http-date: got %v, want ~30s", got)
	}
	if got := parseRetryAfter(nil); got != 0 {
		t.Errorf("nil response: got %v, want 0", got)
	}
}

func TestBackoff_HonorsRetryAfterFloor(t *testing.T) {
	start := time.Now()
	err := backoff(context.Background(), 0, &RateLimitError{RetryAfter: 600 * time.Millisecond}, retryBaseDelay)
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("backoff returned error: %v", err)
	}
	if elapsed < 600*time.Millisecond {
		t.Errorf("expected wait >= 600ms from Retry-After, got %v", elapsed)
	}
}

// TestCappedBackoff_NoOverflowAndBounded: retryMaxAttempts is
// operator-configurable with no upper bound, and a naive
// `baseDelay << attempt` overflows int64 into a NEGATIVE duration at a high
// attempt count, which would panic rand.Int63n. cappedBackoff must stay
// positive and never exceed the ceiling for any attempt count.
func TestCappedBackoff_NoOverflowAndBounded(t *testing.T) {
	// Low attempts grow exponentially from the base.
	if got := cappedBackoff(0, retryBaseDelay); got != retryBaseDelay {
		t.Errorf("attempt 0: got %v, want %v", got, retryBaseDelay)
	}
	if got := cappedBackoff(1, retryBaseDelay); got != 2*retryBaseDelay {
		t.Errorf("attempt 1: got %v, want %v", got, 2*retryBaseDelay)
	}
	// High / overflow-triggering attempts stay positive and capped at the ceiling.
	for _, attempt := range []int{34, 40, 62, 100, 1000} {
		got := cappedBackoff(attempt, retryBaseDelay)
		if got <= 0 {
			t.Errorf("attempt %d: delay must stay positive, got %v (overflow)", attempt, got)
		}
		if got > retryMaxDelay {
			t.Errorf("attempt %d: delay %v exceeds ceiling %v", attempt, got, retryMaxDelay)
		}
	}
}

// TestBackoff_LargeAttemptDoesNotPanic exercises the full backoff() (including
// the rand.Int63n jitter that the old overflow crashed) at a high attempt count.
// A pre-cancelled context returns the moment the bounded delay is computed, so
// the test is fast; the point is that it completes without panicking.
func TestBackoff_LargeAttemptDoesNotPanic(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, attempt := range []int{34, 40, 62, 100, 1000} {
		_ = backoff(ctx, attempt, nil, retryBaseDelay)
	}
}

// TestRetry_Stream_SilentStepDoesNotBlockRetry: a SilentStep marker is
// an idle-watchdog bracket, not a hosted step the provider already ran — a
// stream that emits one and then a retryable error is still replayed.
func TestRetry_Stream_SilentStepDoesNotBlockRetry(t *testing.T) {
	stub := &retryStub{streamPlan: []func(onChunk func(Chunk)) error{
		func(onChunk func(Chunk)) error {
			onChunk(Chunk{SilentStep: 1})
			onChunk(Chunk{Error: fmt.Errorf("%w: slow down", ErrRateLimited)})
			return nil
		},
		func(onChunk func(Chunk)) error {
			onChunk(Chunk{SilentStep: -1})
			onChunk(Chunk{Text: "replayed"})
			onChunk(Chunk{Done: true})
			return nil
		},
	}}
	rp := NewRetryingProviderWithConfig(stub, 3, time.Millisecond)
	var errs []error
	err := rp.Stream(context.Background(), Request{}, func(c Chunk) {
		if c.Error != nil {
			errs = append(errs, c.Error)
		}
	})
	if err != nil {
		t.Fatalf("expected nil return (the replay finished the turn), got %v", err)
	}
	if stub.streamCall != 2 {
		t.Errorf("expected the retry (2 Stream calls), got %d", stub.streamCall)
	}
	// The held error chunk never reached the client: the retry succeeded.
	if len(errs) != 0 {
		t.Errorf("the held error must be dropped by the successful retry, got %v", errs)
	}
}

// namedRetryStub is a retryStub the traced and audited wrappers can sit on
// (they read the provider's ID and token estimate).
type namedRetryStub struct{ retryStub }

func (s *namedRetryStub) ID() string                  { return "stub" }
func (s *namedRetryStub) EstimateTokens(t string) int { return len(t) / 4 }

// TestRetry_Stream_NoticeDoesNotBlockRetry (A3): a provider Notice (the
// reasoning_off status line) is not delivered content — a stream that emits
// one and then a retryable server_error before any text is still replayed —
// and the notice reaches the caller unchanged through the traced, retry and
// audited wrappers.
func TestRetry_Stream_NoticeDoesNotBlockRetry(t *testing.T) {
	notice := &Notice{Name: "reasoning_off", Label: "answering with reasoning off"}
	stub := &namedRetryStub{retryStub{streamPlan: []func(onChunk func(Chunk)) error{
		func(onChunk func(Chunk)) error {
			onChunk(Chunk{Notice: notice})
			onChunk(Chunk{Error: fmt.Errorf("%w: server_error", ErrProviderDown)})
			return nil
		},
		func(onChunk func(Chunk)) error {
			onChunk(Chunk{Notice: notice})
			onChunk(Chunk{Text: "replayed"})
			onChunk(Chunk{Done: true})
			return nil
		},
	}}}
	chain := NewAuditedProvider(NewRetryingProviderWithConfig(NewTracedProvider(stub), 3, time.Millisecond), nil, "user-1", "stub")
	var notices []*Notice
	var text string
	var errs []error
	err := chain.Stream(context.Background(), Request{Model: "m"}, func(c Chunk) {
		if c.Notice != nil {
			notices = append(notices, c.Notice)
		}
		if c.ToolEvent != nil {
			t.Errorf("a notice must not surface as a hosted tool step: %+v", c.ToolEvent)
		}
		if c.Error != nil {
			errs = append(errs, c.Error)
		}
		text += c.Text
	})
	if err != nil {
		t.Fatalf("expected nil return (the replay finished the turn), got %v", err)
	}
	if stub.streamCall != 2 {
		t.Errorf("expected the retry (2 Stream calls), got %d", stub.streamCall)
	}
	if len(errs) != 0 {
		t.Errorf("the held error must be dropped by the successful retry, got %v", errs)
	}
	if text != "replayed" {
		t.Errorf("text = %q, want the replayed answer", text)
	}
	// Both attempts' notices pass through, unchanged (same pointer).
	if len(notices) != 2 || notices[0] != notice || notices[1] != notice {
		t.Errorf("notices = %+v, want the provider's notice from each attempt", notices)
	}
}
