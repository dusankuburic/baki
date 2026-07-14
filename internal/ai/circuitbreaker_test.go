package ai

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// cbStub is a minimal Provider stub for circuit breaker tests.
type cbStub struct {
	Provider
	mu       sync.Mutex
	chatErrs []error
	calls    int
}

func (s *cbStub) Chat(_ context.Context, _ Request) (*Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	i := s.calls
	s.calls++
	if i < len(s.chatErrs) && s.chatErrs[i] != nil {
		return nil, s.chatErrs[i]
	}
	return &Response{Content: "ok"}, nil
}

func (s *cbStub) ID() string { return "stub" }

func newCBStub(errs ...error) *cbStub { return &cbStub{chatErrs: errs} }

func cbCall(t *testing.T, cb *CircuitBreakerProvider) error {
	t.Helper()
	_, err := cb.Chat(context.Background(), Request{})
	return err
}

// cbStreamStub replays a fixed chunk sequence and returns nil, mimicking the
// SSE parsers' behavior of delivering upstream errors as Chunk{Error}.
type cbStreamStub struct {
	Provider
	chunks []Chunk
}

func (s *cbStreamStub) Stream(_ context.Context, _ Request, onChunk func(Chunk)) error {
	for _, c := range s.chunks {
		onChunk(c)
	}
	return nil
}

func (s *cbStreamStub) ID() string { return "stream-stub" }

// TestCircuitBreaker_StreamErrorChunkTripsCircuit: a stream that ends with a
// retryable error chunk (and a nil return) must count as a failure — before
// the fix it was recorded as a success, so a provider that always streamed an
// error event could never open the circuit.
func TestCircuitBreaker_StreamErrorChunkTripsCircuit(t *testing.T) {
	resetBreakerRegistry()
	stub := &cbStreamStub{chunks: []Chunk{
		{Text: "partial"},
		{Error: fmt.Errorf("%w: overloaded", ErrProviderDown)},
	}}
	cb := NewCircuitBreakerProvider(stub)

	for range cbFailureThreshold {
		if err := cb.Stream(context.Background(), Request{}, func(Chunk) {}); err != nil {
			t.Fatalf("Stream returned %v, want nil (the error travels as a chunk)", err)
		}
	}
	err := cb.Stream(context.Background(), Request{}, func(Chunk) {})
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen after %d error-chunk streams, got %v", cbFailureThreshold, err)
	}
}

// TestCircuitBreaker_StreamPermanentErrorChunkDoesNotTrip mirrors record()'s
// closed-circuit rule: non-retryable errors (bad request, cancel) must not
// open the circuit for everyone.
func TestCircuitBreaker_StreamPermanentErrorChunkDoesNotTrip(t *testing.T) {
	resetBreakerRegistry()
	stub := &cbStreamStub{chunks: []Chunk{{Error: errors.New("invalid request")}}}
	cb := NewCircuitBreakerProvider(stub)

	for range cbFailureThreshold + 1 {
		if err := cb.Stream(context.Background(), Request{}, func(Chunk) {}); err != nil {
			t.Fatalf("expected circuit to stay closed on permanent error chunks, got %v", err)
		}
	}
}

// TestCircuitBreaker_StaysClosedBelowThreshold verifies that fewer than
// cbFailureThreshold consecutive failures leave the circuit closed.
func TestCircuitBreaker_StaysClosedBelowThreshold(t *testing.T) {
	errs := make([]error, cbFailureThreshold-1)
	for i := range errs {
		errs[i] = ErrProviderDown
	}
	resetBreakerRegistry()
	cb := NewCircuitBreakerProvider(newCBStub(errs...))

	for range cbFailureThreshold - 1 {
		if err := cbCall(t, cb); !errors.Is(err, ErrProviderDown) {
			t.Fatalf("expected ErrProviderDown, got %v", err)
		}
	}
	// Next call hits the stub's success response (no more errors queued).
	if err := cbCall(t, cb); err != nil {
		t.Fatalf("expected success on closed circuit, got %v", err)
	}
}

// TestCircuitBreaker_OpensAtThreshold verifies that exactly cbFailureThreshold
// consecutive retryable failures open the circuit.
func TestCircuitBreaker_OpensAtThreshold(t *testing.T) {
	errs := make([]error, cbFailureThreshold)
	for i := range errs {
		errs[i] = ErrProviderDown
	}
	resetBreakerRegistry()
	cb := NewCircuitBreakerProvider(newCBStub(errs...))

	for range cbFailureThreshold {
		cbCall(t, cb)
	}

	err := cbCall(t, cb)
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen after threshold, got %v", err)
	}
}

// TestCircuitBreaker_PermanentErrorsDoNotTrip verifies that non-retryable errors
// (e.g. ErrApiKeyInvalid) never open the circuit.
func TestCircuitBreaker_PermanentErrorsDoNotTrip(t *testing.T) {
	// Flood with permanent errors — well over threshold.
	errs := make([]error, cbFailureThreshold*3)
	for i := range errs {
		errs[i] = ErrApiKeyInvalid
	}
	resetBreakerRegistry()
	cb := NewCircuitBreakerProvider(newCBStub(errs...))

	for range cbFailureThreshold * 3 {
		if err := cbCall(t, cb); !errors.Is(err, ErrApiKeyInvalid) {
			t.Fatalf("unexpected error: %v", err)
		}
	}
	// Circuit must still be closed; the next success should go through.
	if err := cbCall(t, cb); err != nil {
		t.Fatalf("circuit should be closed after only permanent errors, got %v", err)
	}
}

// TestCircuitBreaker_RateLimitedDoesNotTrip verifies that 429 (ErrRateLimited)
// — though retryable — does NOT count toward the circuit-open threshold. A 429
// is per-API-key, not "provider down for everyone"; in multi-tenant deployments
// one tenant's rate-limit must not trip the shared breaker and block healthy
// tenants. The retry layer still backs off on 429; only the breaker ignores it.
func TestCircuitBreaker_RateLimitedDoesNotTrip(t *testing.T) {
	errs := make([]error, cbFailureThreshold*3)
	for i := range errs {
		errs[i] = ErrRateLimited
	}
	resetBreakerRegistry()
	cb := NewCircuitBreakerProvider(newCBStub(errs...))

	for range cbFailureThreshold * 3 {
		if err := cbCall(t, cb); !errors.Is(err, ErrRateLimited) {
			t.Fatalf("expected ErrRateLimited, got %v", err)
		}
	}
	// Circuit must still be closed — 429s must not trip it.
	if err := cbCall(t, cb); err != nil {
		t.Fatalf("circuit should be closed after only 429s, got %v (circuit may have opened)", err)
	}
}

// TestCircuitBreaker_ClosesOnSuccessAfterOpen verifies the half-open → closed path.
func TestCircuitBreaker_ClosesOnSuccessAfterOpen(t *testing.T) {
	errs := make([]error, cbFailureThreshold)
	for i := range errs {
		errs[i] = ErrProviderDown
	}
	resetBreakerRegistry()
	cb := NewCircuitBreakerProvider(newCBStub(errs...))

	// Open the circuit.
	for range cbFailureThreshold {
		cbCall(t, cb)
	}
	if !errors.Is(cbCall(t, cb), ErrCircuitOpen) {
		t.Fatal("circuit should be open")
	}

	// Fast-forward past the open window.
	cb.st.mu.Lock()
	cb.st.lastFailure = time.Now().Add(-cbOpenDuration - time.Millisecond)
	cb.st.mu.Unlock()

	// Probe succeeds → circuit closes.
	if err := cbCall(t, cb); err != nil {
		t.Fatalf("half-open probe should succeed, got %v", err)
	}
	// Now fully closed — subsequent calls pass through.
	if err := cbCall(t, cb); err != nil {
		t.Fatalf("closed circuit should pass calls through, got %v", err)
	}
}

// TestCircuitBreaker_RetripsOnFailedProbe verifies the half-open → open path.
func TestCircuitBreaker_RetripsOnFailedProbe(t *testing.T) {
	// First batch: trip the circuit.
	errs := make([]error, cbFailureThreshold+1)
	for i := range errs {
		errs[i] = ErrProviderDown
	}
	resetBreakerRegistry()
	cb := NewCircuitBreakerProvider(newCBStub(errs...))

	for range cbFailureThreshold {
		cbCall(t, cb)
	}

	// Fast-forward past open window to enter half-open.
	cb.st.mu.Lock()
	cb.st.lastFailure = time.Now().Add(-cbOpenDuration - time.Millisecond)
	cb.st.mu.Unlock()

	// Probe fails (stub still has one error queued) → circuit reopens.
	if err := cbCall(t, cb); !errors.Is(err, ErrProviderDown) {
		t.Fatalf("half-open probe expected ErrProviderDown, got %v", err)
	}

	// Circuit should be open again.
	if err := cbCall(t, cb); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen after failed probe, got %v", err)
	}
}

// TestCircuitBreaker_NonRetryableProbeDoesNotWedge verifies that a half-open
// probe failing with a NON-retryable error (e.g. a context timeout or cancelled
// stream) reopens the circuit instead of leaving it stuck half-open forever —
// which would reject every subsequent call to the provider until restart.
func TestCircuitBreaker_NonRetryableProbeDoesNotWedge(t *testing.T) {
	// Trip the circuit with retryable failures, then queue a non-retryable
	// error for the half-open probe (the stub returns successes afterwards).
	errs := make([]error, cbFailureThreshold)
	for i := range errs {
		errs[i] = ErrProviderDown
	}
	errs = append(errs, context.DeadlineExceeded) // the half-open probe
	resetBreakerRegistry()
	cb := NewCircuitBreakerProvider(newCBStub(errs...))

	for range cbFailureThreshold {
		cbCall(t, cb)
	}
	if !errors.Is(cbCall(t, cb), ErrCircuitOpen) {
		t.Fatal("circuit should be open after threshold")
	}

	// Enter half-open and let the single probe fail with the non-retryable error.
	cb.st.mu.Lock()
	cb.st.lastFailure = time.Now().Add(-cbOpenDuration - time.Millisecond)
	cb.st.mu.Unlock()
	if err := cbCall(t, cb); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("half-open probe expected context.DeadlineExceeded, got %v", err)
	}

	// Before the fix this wedged half-open. It must instead be open (cooling
	// down): subsequent callers get ErrCircuitOpen, and after the cooldown a
	// fresh probe is admitted and can recover.
	if !errors.Is(cbCall(t, cb), ErrCircuitOpen) {
		t.Fatal("circuit should be open (cooling down) after a failed probe, not wedged half-open")
	}
	cb.st.mu.Lock()
	cb.st.lastFailure = time.Now().Add(-cbOpenDuration - time.Millisecond)
	cb.st.mu.Unlock()
	if err := cbCall(t, cb); err != nil {
		t.Fatalf("circuit should admit a fresh probe and recover, got %v", err)
	}
}

// TestCircuitBreaker_PersistsAcrossInstances verifies that breaker state is
// shared per provider ID, so failures accumulated through one
// CircuitBreakerProvider open the circuit for a *separate* instance wrapping the
// same provider. This mirrors ProviderFactory.For rebuilding the decorator chain
// on every request: without shared state the breaker could never open.
func TestCircuitBreaker_PersistsAcrossInstances(t *testing.T) {
	resetBreakerRegistry()

	// Each "request" gets a fresh chain (new stub + new breaker), all keyed by
	// the same provider ID ("stub"). Drive cbFailureThreshold failures, one per
	// fresh instance, exactly as repeated For() calls would.
	for range cbFailureThreshold {
		cb := NewCircuitBreakerProvider(newCBStub(ErrProviderDown))
		cbCall(t, cb)
	}

	// A brand-new instance must now see the circuit open.
	cb := NewCircuitBreakerProvider(newCBStub(ErrProviderDown))
	if err := cbCall(t, cb); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("expected ErrCircuitOpen on a fresh instance after threshold reached across instances, got %v", err)
	}
}

// TestCircuitBreaker_ErrCircuitOpenIsNotRetryable ensures that ErrCircuitOpen
// is not retried by RetryingProvider (it must not be in the retryable set).
func TestCircuitBreaker_ErrCircuitOpenIsNotRetryable(t *testing.T) {
	if isRetryable(ErrCircuitOpen) {
		t.Error("ErrCircuitOpen must not be retryable — it would cause a tight loop")
	}
}
