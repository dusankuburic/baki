package ai

import (
	"context"
	"errors"
	"math/rand"
	"time"
)

// retryMaxAttempts is the total number of tries (1 initial + retries).
const retryMaxAttempts = 3

// retryBaseDelay is the first backoff interval; it doubles each attempt.
const retryBaseDelay = 500 * time.Millisecond

// isRetryable reports whether an error is a transient provider condition worth
// retrying. Rate-limit and provider-down are safe to retry; auth/balance/
// context-limit errors are permanent and must surface immediately.
func isRetryable(err error) bool {
	return errors.Is(err, ErrRateLimited) || errors.Is(err, ErrProviderDown)
}

// backoff sleeps before the next retry, returning early (with ctx.Err()) if the
// context is cancelled during the wait. The base wait is an exponentially
// increasing, jittered interval; if the previous error carried a server
// Retry-After hint, the wait is raised to at least that hint so we respect the
// provider's guidance instead of hammering it on a fixed schedule.
func backoff(ctx context.Context, attempt int, lastErr error) error {
	d := retryBaseDelay << attempt // 500ms, 1s, 2s, …
	// Full jitter: random in [0, d] to avoid thundering-herd alignment.
	d = time.Duration(rand.Int63n(int64(d) + 1))
	if hint := retryAfterFrom(lastErr); hint > d {
		d = hint
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// RetryingProvider wraps a Provider with exponential-backoff retries on
// transient errors. It is a decorator (like TracedProvider) applied in the
// factory chain.
type RetryingProvider struct {
	Provider
}

// NewRetryingProvider wraps p so transient failures are retried.
func NewRetryingProvider(p Provider) *RetryingProvider {
	return &RetryingProvider{Provider: p}
}

func (rp *RetryingProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	var lastErr error
	for attempt := range retryMaxAttempts {
		if attempt > 0 {
			if err := backoff(ctx, attempt-1, lastErr); err != nil {
				return nil, err
			}
		}
		resp, err := rp.Provider.Chat(ctx, req)
		if err == nil || !isRetryable(err) {
			return resp, err
		}
		lastErr = err
	}
	return nil, lastErr
}

func (rp *RetryingProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	var lastErr error
	for attempt := range retryMaxAttempts {
		if attempt > 0 {
			if err := backoff(ctx, attempt-1, lastErr); err != nil {
				return nil, err
			}
		}
		res, err := rp.Provider.Embed(ctx, text)
		if err == nil || !isRetryable(err) {
			return res, err
		}
		lastErr = err
	}
	return nil, lastErr
}

// Stream retries only when the underlying provider fails BEFORE emitting any
// chunk. Once a chunk has been delivered to the caller, a partial stream cannot
// be safely replayed, so the error is returned as-is.
func (rp *RetryingProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	var lastErr error
	for attempt := range retryMaxAttempts {
		if attempt > 0 {
			if err := backoff(ctx, attempt-1, lastErr); err != nil {
				return err
			}
		}
		emitted := false
		err := rp.Provider.Stream(ctx, req, func(c Chunk) {
			emitted = true
			onChunk(c)
		})
		if err == nil || !isRetryable(err) || emitted {
			return err
		}
		lastErr = err
	}
	return lastErr
}
