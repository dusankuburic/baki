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
func backoff(ctx context.Context, attempt int, lastErr error, baseDelay time.Duration) error {
	d := baseDelay << attempt // 500ms, 1s, 2s, …
	// Full jitter: random in [0, d] to avoid thundering-herd alignment.
	d = time.Duration(rand.Int63n(int64(d) + 1)) // #nosec G404 -- jitter only, not security-sensitive
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
	maxAttempts int
	baseDelay   time.Duration
}

func NewRetryingProvider(p Provider) *RetryingProvider {
	return &RetryingProvider{
		Provider:    p,
		maxAttempts: retryMaxAttempts,
		baseDelay:   retryBaseDelay,
	}
}

func NewRetryingProviderWithConfig(p Provider, maxAttempts int, baseDelay time.Duration) *RetryingProvider {
	return &RetryingProvider{
		Provider:    p,
		maxAttempts: maxAttempts,
		baseDelay:   baseDelay,
	}
}

func (rp *RetryingProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	var lastErr error
	for attempt := range rp.maxAttempts {
		if attempt > 0 {
			if err := backoff(ctx, attempt-1, lastErr, rp.baseDelay); err != nil {
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
	for attempt := range rp.maxAttempts {
		if attempt > 0 {
			if err := backoff(ctx, attempt-1, lastErr, rp.baseDelay); err != nil {
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

// Stream retries only when the stream fails before any content reaches the
// caller. A provider can fail two ways: by returning an error, or by emitting
// an error Chunk mid-SSE (e.g. Claude's `error` event) and returning nil. An
// error chunk that arrives before any content is held back and converted into
// a returned error so it retries like a pre-stream failure. Once content
// (text, done, or tool calls — metadata-only chunks like TokensIn don't count)
// has been delivered, a partial stream cannot be safely replayed, so errors
// are passed through as-is.
func (rp *RetryingProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	var lastErr error
	for attempt := range rp.maxAttempts {
		if attempt > 0 {
			if err := backoff(ctx, attempt-1, lastErr, rp.baseDelay); err != nil {
				return err
			}
		}
		delivered := false
		var heldErr error
		err := rp.Provider.Stream(ctx, req, func(c Chunk) {
			if c.Error != nil && !delivered {
				heldErr = c.Error
				return
			}
			if c.Text != "" || c.Done || len(c.ToolCalls) > 0 {
				delivered = true
			}
			onChunk(c)
		})
		if err == nil {
			err = heldErr
		}
		if err == nil || !isRetryable(err) || delivered {
			return err
		}
		lastErr = err
	}
	return lastErr
}
