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
	cbFailureThreshold = 5              // consecutive retryable failures before opening
	cbOpenDuration     = 30 * time.Second // minimum time before allowing a probe
)

type circuitState int

const (
	circuitClosed   circuitState = iota
	circuitOpen
	circuitHalfOpen
)

// CircuitBreakerProvider wraps a Provider and opens the circuit after
// cbFailureThreshold consecutive retryable failures. While open, calls fail
// immediately with ErrCircuitOpen (which is not retryable) to avoid hammering
// a known-down provider. After cbOpenDuration the circuit enters half-open and
// allows one probe — success closes it, another failure reopens it.
//
// Only retryable errors (ErrRateLimited, ErrProviderDown) count toward the
// threshold; permanent errors like ErrApiKeyInvalid do not trip the circuit.
type CircuitBreakerProvider struct {
	Provider
	mu          sync.Mutex
	state       circuitState
	failures    int
	lastFailure time.Time
}

func NewCircuitBreakerProvider(p Provider) *CircuitBreakerProvider {
	return &CircuitBreakerProvider{Provider: p}
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
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if cb.state == circuitOpen {
		if time.Since(cb.lastFailure) >= cbOpenDuration {
			cb.transitionLocked(circuitHalfOpen)
			return nil
		}
		return ErrCircuitOpen
	}
	return nil
}

// record updates the failure count and circuit state based on a call outcome.
func (cb *CircuitBreakerProvider) record(err error) {
	cb.mu.Lock()
	defer cb.mu.Unlock()
	if err == nil {
		cb.transitionLocked(circuitClosed)
		cb.failures = 0
		return
	}
	if !isRetryable(err) {
		return
	}
	cb.failures++
	cb.lastFailure = time.Now()
	if cb.failures >= cbFailureThreshold {
		cb.transitionLocked(circuitOpen)
	}
}

// transitionLocked sets the circuit to next, emitting a log line and metric only
// when the state actually changes. Caller must hold cb.mu.
func (cb *CircuitBreakerProvider) transitionLocked(next circuitState) {
	if cb.state == next {
		return
	}
	cb.state = next
	provider := cb.Provider.ID()
	metrics.RecordCircuitBreakerTransition(provider, next.String())
	if next == circuitClosed {
		logger.Info("AI circuit breaker closed", "provider", provider)
	} else {
		logger.Warn("AI circuit breaker transition", "provider", provider, "state", next.String(), "failures", cb.failures)
	}
}
