package ai

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"math/rand"
	"net"
	"time"
)

// retryMaxAttempts is the total number of tries (1 initial + retries).
const retryMaxAttempts = 3

// retryBaseDelay is the first backoff interval; it doubles each attempt.
const retryBaseDelay = 500 * time.Millisecond

// retryMaxDelay caps a single backoff wait — essential, not cosmetic: retryMaxAttempts is
// operator-configurable with no bound, and a naive `baseDelay << attempt` overflows int64 at ~34 attempts
// into a NEGATIVE duration, which panics rand.Int63n.
const retryMaxDelay = 30 * time.Second

// isTransportError reports whether err is a network-level failure of the provider's endpoint (the
// *url.Error/*net.OpError family client.Do returns). The CALLER ending its own request is excluded: a
// cancelled context also surfaces through url.Error, and the user giving up is not an outage.
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

// asProviderDown gives a bare transport error ErrProviderDown semantics, keeping the network error's own
// text visible — the part the user can act on. Already-classified errors pass through unchanged.
func asProviderDown(err error) error {
	if !isTransportError(err) || errors.Is(err, ErrProviderDown) {
		return err
	}
	return fmt.Errorf("%w: %w", ErrProviderDown, err)
}

// isRetryable reports whether an error is a transient provider condition worth retrying: rate-limit,
// provider-down and transport failures (isTransportError); auth/balance/context-limit errors are permanent.
func isRetryable(err error) bool {
	return errors.Is(err, ErrRateLimited) || errors.Is(err, ErrProviderDown) || isTransportError(err)
}

// cappedBackoff returns the pre-jitter exponential backoff for attempt, grown by guarded doubling up to a
// ceiling (see retryMaxDelay for why the guard matters). A base delay above the ceiling is honored, not grown.
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

// maxRetryAfterWait caps how long a server Retry-After hint may hold a call: a longer hint (a TPM window, a
// daily quota's "retry after 86400 seconds") is reported at once as the rate limit it is.
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

// RetryingProvider wraps a Provider with exponential-backoff retries on transient errors; a decorator
// (like TracedProvider) applied in the factory chain.
type RetryingProvider struct {
	Provider
	// providerID is the metric label, resolved once at construction — see
	// metricID and safeProviderID.
	providerID  string
	maxAttempts int
	baseDelay   time.Duration
}

// metricID is the provider label for retry metrics, resolved once at construction. Deliberately NOT
// rp.Provider.ID() on the retry path: a wrapper holding a nil inner provider would turn a retry into a
// panic, and a metric label must never crash the request it describes.
func (rp *RetryingProvider) metricID() string {
	if rp.providerID == "" {
		return "unknown"
	}
	return rp.providerID
}

// safeProviderID reads p.ID() defensively at wiring time: a test double embedding a nil Provider would
// panic otherwise, and construction is not a place to fail for a label.
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

// Chat retries a retryable failure. A failed attempt the provider billed (its error carries usage,
// heldUsageError) is not lost to the attempt after it: its usage is added to the call's — onto the response
// when a later attempt succeeds, onto the error otherwise (R-budget-1).
func (rp *RetryingProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	var lastErr error
	// discarded is the usage of the attempts already retried; the last failed attempt's usage stays on lastErr
	// and joins it only once its retry starts, so no path counts an attempt twice.
	var discarded attemptUsage
	for attempt := range rp.maxAttempts {
		if attempt > 0 {
			recordAIRetryAttempt(rp.metricID(), retryReason(lastErr))
			if err := backoff(ctx, attempt-1, lastErr, rp.baseDelay); err != nil {
				return nil, discarded.onto(err)
			}
			if in, out, served, ok := usageOf(lastErr); ok {
				discarded.add(in, out, served)
			}
		}
		resp, err := rp.Provider.Chat(ctx, req)
		if err == nil {
			if resp != nil && (discarded.in > 0 || discarded.out > 0) {
				billed := *resp
				billed.TokensIn += discarded.in
				billed.TokensOut += discarded.out
				resp = &billed
			}
			return resp, nil
		}
		if !isRetryable(err) || replayForbidden(err) {
			return resp, discarded.onto(err)
		}
		lastErr = err
	}
	// A transport failure (connection refused, DNS) only reaches this point
	// after exhausting the budget: hand it on with provider-down semantics.
	return nil, discarded.onto(asProviderDown(lastErr))
}

// attemptUsage is the usage of the attempts a retry discarded.
type attemptUsage struct {
	in, out int
	served  string
}

func (u *attemptUsage) add(in, out int, served string) {
	u.in += in
	u.out += out
	if served != "" {
		u.served = served
	}
}

// onto returns err carrying the discarded attempts' usage on top of its own (heldUsageError); err unchanged when
// none was discarded. The last attempt's usage was not added, so nothing is counted twice.
func (u attemptUsage) onto(err error) error {
	if err == nil || (u.in == 0 && u.out == 0) {
		return err
	}
	in, out, served, _ := usageOf(err)
	if served == "" {
		served = u.served
	}
	return &heldUsageError{err: err, tokensIn: in + u.in, tokensOut: out + u.out, servedModel: served}
}

// retryReason maps a retryable error onto a CLOSED-SET metric label — raw error text is never used: it
// carries provider-specific detail and would make the counter unbounded.
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
			recordAIRetryAttempt(rp.metricID(), retryReason(lastErr))
			if err := backoff(ctx, attempt-1, lastErr, rp.baseDelay); err != nil {
				return nil, err
			}
		}
		res, err := rp.Provider.Embed(ctx, text)
		if err == nil || !isRetryable(err) || replayForbidden(err) {
			return res, err
		}
		lastErr = err
	}
	return nil, asProviderDown(lastErr)
}

// heldUsageError carries the usage of a withheld error chunk (see Stream) onto the error Stream finally
// returns, so the audited provider can still bill a run whose chunks it never saw. Classification unchanged.
type heldUsageError struct {
	err         error
	tokensIn    int
	tokensOut   int
	servedModel string
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

// Stream retries only when the stream fails before any content reaches the caller: an error chunk arriving
// before content is held back and returned as an error, so it retries like a pre-stream failure. Once
// content (text, done, tool calls or a hosted server-side step — metadata-only chunks don't count) is
// delivered, a partial stream cannot be safely replayed and errors pass through. A withheld error chunk's
// usage rides on the final error via heldUsageError.
func (rp *RetryingProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	var lastErr error
	// discarded is the usage of the attempt being retried: reported as an attemptDiscarded chunk once the next
	// attempt starts, so the audited wrapper records it as its own row instead of letting the next attempt's
	// counts overwrite it (R-budget-1). The last attempt's usage stays on its chunks and its error, as before.
	var discarded attemptUsage
	for attempt := range rp.maxAttempts {
		if attempt > 0 {
			recordAIRetryAttempt(rp.metricID(), retryReason(lastErr))
			if err := backoff(ctx, attempt-1, lastErr, rp.baseDelay); err != nil {
				// The retry will not run (a Stop, a deadline, a long Retry-After): the error carries the
				// attempt's usage, a held error's included, unless it already does.
				if _, _, _, ok := usageOf(err); !ok && (discarded.in > 0 || discarded.out > 0) {
					return &heldUsageError{err: err, tokensIn: discarded.in, tokensOut: discarded.out, servedModel: discarded.served}
				}
				return err
			}
			if discarded.in > 0 || discarded.out > 0 || discarded.served != "" {
				onChunk(Chunk{attemptDiscarded: true, TokensIn: discarded.in, TokensOut: discarded.out, ServedModel: discarded.served})
			}
			discarded = attemptUsage{}
		}
		// The usage this attempt reported on its forwarded chunks (a message_start before the error).
		var seen attemptUsage
		delivered := false
		// silentOpen is this attempt's net SilentStep: a failed attempt's open phase is closed before the next
		// one starts, or the chat's idle watchdog keeps the hosted-step limit for the rest of the call (R-provider-7).
		silentOpen := 0
		var heldErr error
		var heldTokensIn, heldTokensOut int
		var heldServedModel string
		err := rp.Provider.Stream(ctx, req, func(c Chunk) {
			if c.Error != nil && !delivered {
				heldErr = c.Error
				heldTokensIn, heldTokensOut, heldServedModel = c.TokensIn, c.TokensOut, c.ServedModel
				return
			}
			// A hosted tool step also counts — shown in the trail (ToolEvent) or not (ServerStep): the
			// provider already ran something server-side, so the turn cannot be replayed.
			if c.Text != "" || c.Done || len(c.ToolCalls) > 0 || c.ToolEvent != nil || c.ServerStep {
				delivered = true
			}
			silentOpen += int(c.SilentStep)
			seen.in, seen.out = max(seen.in, c.TokensIn), max(seen.out, c.TokensOut)
			if c.ServedModel != "" {
				seen.served = c.ServedModel
			}
			onChunk(c)
		})
		if err == nil {
			err = heldErr
			if err != nil && (heldTokensIn > 0 || heldTokensOut > 0 || heldServedModel != "") {
				err = &heldUsageError{err: err, tokensIn: heldTokensIn, tokensOut: heldTokensOut, servedModel: heldServedModel}
			}
		}
		if err == nil || !isRetryable(err) || replayForbidden(err) || delivered {
			return err
		}
		for ; silentOpen > 0; silentOpen-- {
			onChunk(Chunk{SilentStep: -1})
		}
		discarded = attemptUsage{in: max(seen.in, heldTokensIn), out: max(seen.out, heldTokensOut), served: cmp.Or(heldServedModel, seen.served)}
		lastErr = err
	}
	return asProviderDown(lastErr)
}
