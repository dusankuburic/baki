package ai

import (
	"context"
	"errors"
	"testing"
	"time"
)

// TestBackoff_LongRetryAfterHintReturnsTheRateLimit (PERF-2): a Retry-After
// beyond maxRetryAfterWait is not slept on — the rate limit is returned at
// once instead of parking the call past the connection test's budget or the
// idle watchdog.
func TestBackoff_LongRetryAfterHintReturnsTheRateLimit(t *testing.T) {
	hint := &RateLimitError{RetryAfter: maxRetryAfterWait + time.Second}
	start := time.Now()
	err := backoff(context.Background(), 0, hint, retryBaseDelay)
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("want the rate limit back, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("backoff slept %v on a hint it cannot honour", elapsed)
	}
}

// TestBackoff_DeadlineTooShortReturnsLastErr (PERF-2): a wait the caller's
// deadline cannot cover ends with the real cause, not with the deadline
// that used to mask it; a wait that fits is still slept.
func TestBackoff_DeadlineTooShortReturnsLastErr(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	hint := &RateLimitError{RetryAfter: 5 * time.Second}
	start := time.Now()
	err := backoff(ctx, 0, hint, retryBaseDelay)
	if !errors.Is(err, ErrRateLimited) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("want the rate limit, got %v", err)
	}
	if elapsed := time.Since(start); elapsed > 200*time.Millisecond {
		t.Errorf("backoff waited %v for a deadline it could not meet", elapsed)
	}

	ctx2, cancel2 := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel2()
	start = time.Now()
	if err := backoff(ctx2, 0, &RateLimitError{RetryAfter: 50 * time.Millisecond}, time.Millisecond); err != nil {
		t.Fatalf("a wait that fits: %v", err)
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Errorf("hint not honoured: waited %v", elapsed)
	}
}

// TestRetry_RateLimitSurfacesWithinTheCallerDeadline (PERF-2): the connection
// test's situation — a 429 with a long Retry-After under a short deadline —
// reports RATE_LIMITED after one call, not "context deadline exceeded" after
// the whole budget.
func TestRetry_RateLimitSurfacesWithinTheCallerDeadline(t *testing.T) {
	limited := &RateLimitError{RetryAfter: 20 * time.Second}
	stub := &retryStub{chatErrs: []error{limited, limited, limited}}
	rp := NewRetryingProviderWithConfig(stub, retryMaxAttempts, retryBaseDelay)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	start := time.Now()
	_, err := rp.Chat(ctx, Request{})
	if !errors.Is(err, ErrRateLimited) {
		t.Fatalf("want ErrRateLimited, got %v", err)
	}
	if stub.chatCalls != 1 {
		t.Errorf("calls = %d, want 1", stub.chatCalls)
	}
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Errorf("took %v; the rate limit must surface at once", elapsed)
	}
}
