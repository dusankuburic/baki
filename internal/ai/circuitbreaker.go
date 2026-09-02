package ai

import (
	"context"
	"errors"
	"sync"
	"time"

	"pad-analyzer/internal/metrics"
	"pad-core/logger"
)

func (s circuitState) String() string {
	switch s {
	case circuitOpen:
		return "open"
	case circuitHalfOpen:
		return "half-open"
	default:
		return "closed"
	}
}

const (
	cbFailureThreshold = 5                // consecutive retryable failures before opening
	cbOpenDuration     = 30 * time.Second // minimum time before allowing a probe
)

type circuitState int

const (
	circuitClosed circuitState = iota
	circuitOpen
	circuitHalfOpen
)

// breakerState is the circuit state for one upstream provider. It is shared,
// process-wide and keyed by provider ID (see breakerStateFor) so the breaker
// persists across requests even though ProviderFactory.For rebuilds the whole
// decorator chain — and therefore a fresh CircuitBreakerProvider — on every
// call. Without this, the per-instance state reset each request and the breaker
// could never accumulate enough failures to open.
type breakerState struct {
	mu          sync.Mutex
	state       circuitState
	failures    int
	lastFailure time.Time
}

var (
	breakerRegistry   = map[string]*breakerState{}
	breakerRegistryMu sync.Mutex
)

// breakerStateFor returns the shared circuit state for a registry key,
// creating it on first use. State is per upstream (not per user/scope): a
// provider that is down is down for everyone.
func breakerStateFor(key string) *breakerState {
	breakerRegistryMu.Lock()
	defer breakerRegistryMu.Unlock()
	s, ok := breakerRegistry[key]
	if !ok {
		s = &breakerState{}
		breakerRegistry[key] = s
	}
	return s
}

// breakerKey scopes circuit state. Model-level: provider families mix
// per-model health (a deprecated model ID 500-ing while its siblings are
// fine — common on GitHub Models), and a provider-wide key meant five
// consecutive failures on ONE bad model blocked every model and every user
// for the cooldown. Empty model (Embed, model-less requests) falls back to
// the provider-wide key. Registry growth is bounded by distinct models in
// use, which the caller population bounds naturally.
func breakerKey(providerID, model string) string {
	if model == "" {
		return providerID
	}
	return providerID + "|" + model
}

// resetBreakerRegistry clears all shared breaker state. Test-only — it lets a
// test start from a known-closed circuit without leaking state into the next.
func resetBreakerRegistry() {
	breakerRegistryMu.Lock()
	defer breakerRegistryMu.Unlock()
	breakerRegistry = map[string]*breakerState{}
}

// CircuitBreakerProvider wraps a Provider and opens the circuit after
// cbFailureThreshold consecutive retryable failures. While open, calls fail
// immediately with ErrCircuitOpen (which is not retryable) to avoid hammering
// a known-down provider. After cbOpenDuration the circuit enters half-open and
// allows one probe — success closes it, another failure reopens it.
//
// Only retryable errors (ErrRateLimited, ErrProviderDown) count toward the
// threshold; permanent errors like ErrApiKeyInvalid do not trip the circuit.
//
// The mutable state lives in a shared *breakerState (keyed by provider ID), so
// every CircuitBreakerProvider for the same provider — across users and across
// per-request chain rebuilds — observes the same circuit.
type CircuitBreakerProvider struct {
	Provider
	providerID       string
	failureThreshold int
	openDuration     time.Duration
}

// stateFor resolves the shared circuit for this call's scope. Chat/Stream key
// by the request's model (see breakerKey); anything without a model uses the
// provider-wide key.
func (cb *CircuitBreakerProvider) stateFor(model string) *breakerState {
	return breakerStateFor(breakerKey(cb.providerID, model))
}

func NewCircuitBreakerProvider(p Provider) *CircuitBreakerProvider {
	return &CircuitBreakerProvider{
		Provider:         p,
		providerID:       p.ID(),
		failureThreshold: cbFailureThreshold,
		openDuration:     cbOpenDuration,
	}
}

func NewCircuitBreakerProviderWithConfig(p Provider, threshold int, openDur time.Duration) *CircuitBreakerProvider {
	return &CircuitBreakerProvider{
		Provider:         p,
		providerID:       p.ID(),
		failureThreshold: threshold,
		openDuration:     openDur,
	}
}

func (cb *CircuitBreakerProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	st := cb.stateFor(req.Model)
	if err := cb.checkSt(st); err != nil {
		return nil, err
	}
	defer cb.recordPanicSt(st)
	resp, err := cb.Provider.Chat(ctx, req)
	cb.recordSt(st, err)
	return resp, err
}

func (cb *CircuitBreakerProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	st := cb.stateFor("")
	if err := cb.checkSt(st); err != nil {
		return nil, err
	}
	defer cb.recordPanicSt(st)
	res, err := cb.Provider.Embed(ctx, text)
	cb.recordSt(st, err)
	return res, err
}

// recordPanic resolves the circuit when the wrapped provider panics. Without it,
// a panic skips the record() call in each method, so a half-open PROBE is never
// resolved — check() then rejects every subsequent caller to that provider
// forever (wedged until process restart). It records a failure (reopening a
// half-open probe / counting toward the threshold) and re-panics so the original
// crash still propagates to the caller's recover for logging. On the normal
// (no-panic) path recover() returns nil and this is a no-op — the explicit
// record() above already ran, so there is no double-record.
func (cb *CircuitBreakerProvider) recordPanicSt(st *breakerState) {
	if r := recover(); r != nil {
		cb.recordSt(st, ErrProviderDown)
		panic(r)
	}
}

// Stream counts mid-stream failures too: the SSE parsers surface an upstream
// error event as a Chunk{Error} and then return nil, which would otherwise be
// recorded as a success — a provider that always streams an error event could
// never trip the breaker. record() still filters by isRetryable, so permanent
// or caller-cancelled errors don't open the circuit.
func (cb *CircuitBreakerProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	st := cb.stateFor(req.Model)
	if err := cb.checkSt(st); err != nil {
		return err
	}
	defer cb.recordPanicSt(st)
	var chunkErr error
	err := cb.Provider.Stream(ctx, req, func(c Chunk) {
		if c.Error != nil {
			chunkErr = c.Error
		}
		onChunk(c)
	})
	outcome := err
	if outcome == nil {
		outcome = chunkErr
	}
	cb.recordSt(st, outcome)
	return err
}

// check returns ErrCircuitOpen when the circuit is open and the cooldown has
// not elapsed yet. In half-open state only ONE probe is allowed through;
// subsequent callers are rejected until the probe completes via record().
func (cb *CircuitBreakerProvider) checkSt(st *breakerState) error {
	st.mu.Lock()
	defer st.mu.Unlock()
	switch st.state {
	case circuitOpen:
		if time.Since(st.lastFailure) >= cb.openDuration {
			cb.transitionLocked(st, circuitHalfOpen)
			return nil // allow the single probe
		}
		return ErrCircuitOpen
	case circuitHalfOpen:
		// A probe is already in flight — reject all others until it resolves.
		return ErrCircuitOpen
	default:
		return nil
	}
}

// record updates the failure count and circuit state based on a call outcome.
func (cb *CircuitBreakerProvider) recordSt(st *breakerState, err error) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if err == nil {
		// Any success (including the half-open probe) closes the circuit.
		cb.transitionLocked(st, circuitClosed)
		st.failures = 0
		return
	}
	// A failed half-open probe must always resolve the probe — otherwise the
	// circuit stays half-open and checkSt() rejects every caller forever.
	// Reopen and restart the cooldown regardless of whether the error is
	// "retryable": a timeout/cancel means the provider is still unhealthy, and
	// a permanent error is surfaced to the next admitted probe after the
	// cooldown rather than wedging the breaker until process restart.
	if st.state == circuitHalfOpen {
		st.lastFailure = time.Now()
		cb.transitionLocked(st, circuitOpen)
		return
	}
	// Closed circuit: only transient provider conditions count toward tripping.
	// Permanent errors (bad key, invalid request, user cancellation) must not
	// open the circuit for everyone.
	if !isRetryable(err) {
		return
	}
	// Rate-limit (429) is per-API-key, not "provider down for everyone." In
	// multi-tenant deployments each tenant has their own key, so one noisy
	// tenant's 429s must not trip the shared breaker and block healthy tenants.
	// The retry layer still backs off on 429 (isRetryable returns true); we just
	// don't count it toward the circuit-open threshold.
	if errors.Is(err, ErrRateLimited) {
		return
	}
	st.failures++
	st.lastFailure = time.Now()
	if st.failures >= cb.failureThreshold {
		cb.transitionLocked(st, circuitOpen)
	}
}

// transitionLocked sets the circuit to next, emitting a log line and metric only
// when the state actually changes. Caller must hold cb.st.mu.
func (cb *CircuitBreakerProvider) transitionLocked(st *breakerState, next circuitState) {
	if st.state == next {
		return
	}
	st.state = next
	metrics.RecordCircuitBreakerTransition(cb.providerID, next.String())
	if next == circuitClosed {
		logger.Info("AI circuit breaker closed", "provider", cb.providerID)
	} else {
		logger.Warn("AI circuit breaker transition", "provider", cb.providerID, "state", next.String(), "failures", st.failures)
	}
}
