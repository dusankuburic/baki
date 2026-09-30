package ai

import (
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
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

// isTransportError reports whether err is a network-level failure of the
// provider's endpoint — the *url.Error/*net.OpError family client.Do returns
// for connection refused/reset, DNS failure, an aborted TLS exchange. The
// CALLER ending its own request is deliberately excluded: a cancelled or
// timed-out request context also surfaces through url.Error, and the user
// giving up is not a provider outage worth retrying or tripping on.
func isTransportError(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var dnsErr *net.DNSError
	if errors.As(err, &dnsErr) {
		return true
	}
	var opErr *net.OpError
	return errors.As(err, &opErr)
}

// asProviderDown gives a bare transport error ErrProviderDown semantics, so
// callers (the service's error mapping, logs) classify an unreachable
// provider as down, while the network error's own text — the part the user
// can act on ("connection refused", "no such host") — stays visible. Errors
// already carrying a classification pass through unchanged.
func asProviderDown(err error) error {
	if !isTransportError(err) || errors.Is(err, ErrProviderDown) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrProviderDown, err)
}

// isRetryable reports whether an error is a transient provider condition worth
// retrying. Rate-limit, provider-down and transport-level network failures
// (connection refused/reset, DNS — see isTransportError) are safe to retry;
// auth/balance/context-limit errors are permanent and must surface immediately.
func isRetryable(err error) bool {
	return errors.Is(err, ErrRateLimited) || errors.Is(err, ErrProviderDown) || isTransportError(err)
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

// maxRetryAfterWait caps how long a server Retry-After hint may hold a call.
// A longer hint (a TPM window, a daily quota's "retry after 86400 seconds")
// is reported at once as the rate limit it is instead of outliving the
// 15 s connection test (which then said "context deadline exceeded" for a
// 429 it is meant to count as OK) or the 90 s idle watchdog.
const maxRetryAfterWait = 20 * time.Second

func backoff(ctx context.Context, attempt int, lastErr error, baseDelay time.Duration) error {
	d := cappedBackoff(attempt, baseDelay)
	// Full jitter: random in [0, d] to avoid thundering-herd alignment.
	d = time.Duration(rand.Int63n(int64(d) + 1)) // #nosec G404 -- jitter only, not security-sensitive
	if hint := retryAfterFrom(lastErr); hint > d {
		if hint > maxRetryAfterWait {
			return lastErr
		}
		d = hint
	}
	// A wait the caller's deadline cannot cover ends with the real cause
	// (the rate limit, the outage), not with the deadline that masked it.
	if dl, ok := ctx.Deadline(); ok && lastErr != nil && time.Until(dl) <= d {
		return lastErr
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
	// A transport failure (connection refused, DNS) only reaches this point
	// after exhausting the budget: hand it on with provider-down semantics.
	return nil, asProviderDown(lastErr)
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
	case isTransportError(err):
		return "transport"
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
	return nil, asProviderDown(lastErr)
}

// heldUsageError carries the token usage of a withheld error chunk (see
// Stream) onto the error Stream finally returns, so wrappers further out —
// the audited provider records spend from chunks it never sees — can still
// bill the run. The message and classification pass through unchanged.
type heldUsageError struct {
	err         error
	tokensIn    int
	tokensOut   int
	servedModel string
	// cacheRead/cacheWrite are the cached-input breakdown of tokensIn the
	// withheld chunk carried (see Chunk.CacheReadTokens), so a failed run's
	// usage is still priced at the cached rates. Zero when the provider
	// reported no breakdown.
	cacheRead  int
	cacheWrite int
}

func (e *heldUsageError) Error() string { return e.err.Error() }
func (e *heldUsageError) Unwrap() error { return e.err }

// usageOf extracts the usage a Stream error carried, if any.
func usageOf(err error) (tokensIn, tokensOut int, servedModel string, ok bool) {
	var he *heldUsageError
	if !errors.As(err, &he) {
		return 0, 0, "", false
	}
	return he.tokensIn, he.tokensOut, he.servedModel, true
}

// cacheUsageOf extracts the cached-token breakdown a Stream error carried
// (heldUsageError), if any — the same read as usageOf for the cache slice.
func cacheUsageOf(err error) (read, write int) {
	var he *heldUsageError
	if !errors.As(err, &he) {
		return 0, 0
	}
	return he.cacheRead, he.cacheWrite
}

// Stream retries only when the stream fails before any content reaches the
// caller. A provider can fail two ways: by returning an error, or by emitting
// an error Chunk mid-SSE (e.g. Claude's `error` event) and returning nil. An
// error chunk that arrives before any content is held back and converted into
// a returned error so it retries like a pre-stream failure. Once content
// (text, done, tool calls or a hosted server-side step — metadata-only chunks
// like TokensIn, empty keepalives, split-off Reasoning and provider Notices,
// none of which is answer content, don't count) has been delivered, a partial
// stream cannot be safely replayed, so errors are passed through as-is.
//
// A withheld error chunk may be the only place a run's token usage rides (the
// MCP-approvals error is one such); the usage is stashed and re-attached to
// the error this wrapper finally returns via heldUsageError.
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
		var heldTokensIn, heldTokensOut int
		var heldCacheRead, heldCacheWrite int
		var heldServedModel string
		err := rp.Provider.Stream(ctx, req, func(c Chunk) {
			if c.Error != nil && !delivered {
				heldErr = c.Error
				heldTokensIn, heldTokensOut, heldServedModel = c.TokensIn, c.TokensOut, c.ServedModel
				heldCacheRead, heldCacheWrite = c.CacheReadTokens, c.CacheWriteTokens
				return
			}
			// A hosted tool step also counts — shown in the trail (ToolEvent)
			// or not (ServerStep): the provider already ran something
			// server-side, so the turn cannot be replayed. Replay safety must
			// not depend on which steps have a display label.
			if c.Text != "" || c.Done || len(c.ToolCalls) > 0 || c.ToolEvent != nil || c.ServerStep {
				delivered = true
			}
			onChunk(c)
		})
		if err == nil {
			err = heldErr
			if err != nil && (heldTokensIn > 0 || heldTokensOut > 0 || heldServedModel != "") {
				err = &heldUsageError{err: err, tokensIn: heldTokensIn, tokensOut: heldTokensOut, cacheRead: heldCacheRead, cacheWrite: heldCacheWrite, servedModel: heldServedModel}
			}
		}
		if err == nil || !isRetryable(err) || delivered {
			return err
		}
		lastErr = err
	}
	return asProviderDown(lastErr)
}
