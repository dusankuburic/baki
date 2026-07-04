package ai

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"
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
