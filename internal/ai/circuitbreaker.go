package ai

import (
	"context"
	"sync"
	"time"

	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/metrics"
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

type cbConfig struct {
	FailureThreshold int
	OpenDuration     time.Duration
}

type circuitState int

const (
	circuitClosed   circuitState = iota
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

// breakerStateFor returns the shared circuit state for a provider ID, creating
// it on first use. State is per upstream provider (not per user/scope): a
// provider that is down is down for everyone.
func breakerStateFor(providerID string) *breakerState {
	breakerRegistryMu.Lock()
	defer breakerRegistryMu.Unlock()
	s, ok := breakerRegistry[providerID]
	if !ok {
		s = &breakerState{}
		breakerRegistry[providerID] = s
	}
	return s
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
	st               *breakerState
	failureThreshold int
	openDuration     time.Duration
}

func NewCircuitBreakerProvider(p Provider) *CircuitBreakerProvider {
	return &CircuitBreakerProvider{
		Provider:         p,
		st:               breakerStateFor(p.ID()),
		failureThreshold: cbFailureThreshold,
		openDuration:     cbOpenDuration,
	}
}

func NewCircuitBreakerProviderWithConfig(p Provider, threshold int, openDur time.Duration) *CircuitBreakerProvider {
	return &CircuitBreakerProvider{
		Provider:         p,
		st:               breakerStateFor(p.ID()),
		failureThreshold: threshold,
		openDuration:     openDur,
	}
}

func (cb *CircuitBreakerProvider) Chat(ctx context.Context, req Request) (*Response, error) {
	if err := cb.check(); err != nil {
		return nil, err
	}
	resp, err := cb.Provider.Chat(ctx, req)
	cb.record(err)
	return resp, err
}

func (cb *CircuitBreakerProvider) Embed(ctx context.Context, text []string) ([][]float32, error) {
	if err := cb.check(); err != nil {
		return nil, err
	}
	res, err := cb.Provider.Embed(ctx, text)
	cb.record(err)
	return res, err
}

func (cb *CircuitBreakerProvider) Stream(ctx context.Context, req Request, onChunk func(Chunk)) error {
	if err := cb.check(); err != nil {
		return err
	}
	err := cb.Provider.Stream(ctx, req, onChunk)
	cb.record(err)
	return err
}

// check returns ErrCircuitOpen when the circuit is open and the cooldown has
// not elapsed yet. In half-open state it allows a single probe through.
func (cb *CircuitBreakerProvider) check() error {
	cb.st.mu.Lock()
	defer cb.st.mu.Unlock()
	if cb.st.state == circuitOpen {
		if time.Since(cb.st.lastFailure) >= cb.openDuration {
			cb.transitionLocked(circuitHalfOpen)
			return nil
		}
		return ErrCircuitOpen
	}
	return nil
}

// record updates the failure count and circuit state based on a call outcome.
func (cb *CircuitBreakerProvider) record(err error) {
	cb.st.mu.Lock()
	defer cb.st.mu.Unlock()
	if err == nil {
		cb.transitionLocked(circuitClosed)
		cb.st.failures = 0
		return
	}
	if !isRetryable(err) {
		return
	}
	cb.st.failures++
	cb.st.lastFailure = time.Now()
	if cb.st.failures >= cb.failureThreshold {
		cb.transitionLocked(circuitOpen)
	}
}

// transitionLocked sets the circuit to next, emitting a log line and metric only
// when the state actually changes. Caller must hold cb.st.mu.
func (cb *CircuitBreakerProvider) transitionLocked(next circuitState) {
	if cb.st.state == next {
		return
	}
	cb.st.state = next
	provider := cb.Provider.ID()
	metrics.RecordCircuitBreakerTransition(provider, next.String())
	if next == circuitClosed {
		logger.Info("AI circuit breaker closed", "provider", provider)
	} else {
		logger.Warn("AI circuit breaker transition", "provider", provider, "state", next.String(), "failures", cb.st.failures)
	}
}
