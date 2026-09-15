package ai

import (
	"context"
	"errors"
	"math/rand"
	"pad-analyzer/internal/metrics"
	"time"
)

// retryMaxAttempts is the total number of tries (1 initial + retries).
const retryMaxAttempts = 3

// retryBaseDelay is the first backoff interval; it doubles each attempt.
const retryBaseDelay = 500 * time.Millisecond

// retryMaxDelay caps a single backoff wait. The ceiling is essential, not just
// cosmetic: retryMaxAttempts is operator-configurable (PAD_RETRY_MAX_ATTEMPTS)
// with no upper bound, and a naive `baseDelay << attempt` overflows int64 at
// ~34 attempts into a NEGATIVE duration — which then panics rand.Int63n and
// crashes the request goroutine. Growing the delay by guarded doubling up to
// this ceiling keeps it positive and bounded for any attempt count.
const retryMaxDelay = 30 * time.Second

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
// cappedBackoff returns the pre-jitter exponential backoff delay for attempt,
// grown by guarded doubling up to a ceiling. Doubling-with-guard (rather than
// `baseDelay << attempt`) is essential: retryMaxAttempts is operator-configurable
// (PAD_RETRY_MAX_ATTEMPTS) with no upper bound, and the shift overflows int64
// into a NEGATIVE duration at ~34 attempts — which then panics rand.Int63n in
// backoff. A base delay already above the ceiling is honored but not grown.
func cappedBackoff(attempt int, baseDelay time.Duration) time.Duration {
	if baseDelay <= 0 {
		baseDelay = retryBaseDelay
	}
	ceiling := max(retryMaxDelay, baseDelay)
	d := baseDelay
	for i := 0; i < attempt && d < ceiling; i++ {
		d <<= 1
	}
	if d > ceiling {
		d = ceiling
	}
	return d
}

func backoff(ctx context.Context, attempt int, lastErr error, baseDelay time.Duration) error {
	d := cappedBackoff(attempt, baseDelay)
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
	// providerID is the metric label, resolved once at construction — see
	// metricID and safeProviderID.
	providerID  string
	maxAttempts int
	baseDelay   time.Duration
}

// metricID is the provider label for retry metrics, resolved once at
// construction.
//
// Deliberately NOT rp.Provider.ID() on the retry path: that adds an interface
// call into the wrapped provider at the exact moment something is already going
// wrong, and any wrapper holding a nil inner provider would turn a retry — a
// recoverable condition — into a panic. A metric label must never be able to
// crash the request it is describing. Resolving at construction also keeps the
// hot path free of a call per attempt.
func (rp *RetryingProvider) metricID() string {
	if rp.providerID == "" {
		return "unknown"
	}
	return rp.providerID
}

// safeProviderID reads p.ID() defensively at wiring time. A test double that
// embeds a nil Provider to avoid implementing the full interface would panic
// here otherwise, and construction is not a place to fail for a label.
func safeProviderID(p Provider) (id string) {
	defer func() {
		if recover() != nil {
			id = "unknown"
		}
	}()
	return p.ID()
}

func NewRetryingProvider(p Provider) *RetryingProvider {
	return &RetryingProvider{
		Provider:    p,
		providerID:  safeProviderID(p),
		maxAttempts: retryMaxAttempts,
		baseDelay:   retryBaseDelay,
	}
}

func NewRetryingProviderWithConfig(p Provider, maxAttempts int, baseDelay time.Duration) *RetryingProvider {
	return &RetryingProvider{
		Provider:    p,
		providerID:  safeProviderID(p),
		maxAttempts: maxAttempts,
		baseDelay:   baseDelay,
	}
}

func (rp *RetryingProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	var lastErr error
	for attempt := range rp.maxAttempts {
		if attempt > 0 {
			metrics.RecordAIRetryAttempt(rp.metricID(), retryReason(lastErr))
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

// retryReason maps a retryable error onto a CLOSED-SET metric label. Raw error
// text is never used: it carries provider-specific detail and would make the
// counter unbounded.
func retryReason(err error) string {
	switch {
	case errors.Is(err, ErrRateLimited):
		return "rate_limited"
	case errors.Is(err, ErrProviderDown):
		return "provider_down"
	default:
		return "other"
	}
}

func (rp *RetryingProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	var lastErr error
	for attempt := range rp.maxAttempts {
		if attempt > 0 {
			metrics.RecordAIRetryAttempt(rp.metricID(), retryReason(lastErr))
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
			metrics.RecordAIRetryAttempt(rp.metricID(), retryReason(lastErr))
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
