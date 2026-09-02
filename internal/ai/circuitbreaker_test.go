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
	cb.stateFor("").mu.Lock()
	cb.stateFor("").lastFailure = time.Now().Add(-cbOpenDuration - time.Millisecond)
	cb.stateFor("").mu.Unlock()

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
	cb.stateFor("").mu.Lock()
	cb.stateFor("").lastFailure = time.Now().Add(-cbOpenDuration - time.Millisecond)
	cb.stateFor("").mu.Unlock()

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
	cb.stateFor("").mu.Lock()
	cb.stateFor("").lastFailure = time.Now().Add(-cbOpenDuration - time.Millisecond)
	cb.stateFor("").mu.Unlock()
	if err := cbCall(t, cb); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("half-open probe expected context.DeadlineExceeded, got %v", err)
	}

	// Before the fix this wedged half-open. It must instead be open (cooling
	// down): subsequent callers get ErrCircuitOpen, and after the cooldown a
	// fresh probe is admitted and can recover.
	if !errors.Is(cbCall(t, cb), ErrCircuitOpen) {
		t.Fatal("circuit should be open (cooling down) after a failed probe, not wedged half-open")
	}
	cb.stateFor("").mu.Lock()
	cb.stateFor("").lastFailure = time.Now().Add(-cbOpenDuration - time.Millisecond)
	cb.stateFor("").mu.Unlock()
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

// cbPanicStub is a Provider whose Chat panics — simulating a downstream provider
// crashing mid-call (nil deref, etc.). Used to prove a panic during a half-open
// probe still resolves the circuit rather than wedging it half-open.
type cbPanicStub struct{ Provider }

func (cbPanicStub) Chat(context.Context, Request) (*Response, error) {
	panic("simulated provider crash")
}
func (cbPanicStub) ID() string { return "panic-stub" }

// TestCircuitBreaker_ProbePanicDoesNotWedge is the regression test for the
// panic-skips-record wedge: if the wrapped provider panics during a HALF-OPEN
// probe, record() (called after the provider returns) is skipped, so without the
// recover-and-record guard the circuit stays half-open forever and check()
// rejects every future caller. The fix records a failure on panic and re-panics.
func TestCircuitBreaker_ProbePanicDoesNotWedge(t *testing.T) {
	resetBreakerRegistry()
	cb := NewCircuitBreakerProvider(cbPanicStub{})

	// Drive the shared state to a half-open probe: open with an elapsed cooldown
	// so the next check() admits exactly one probe.
	st := cb.stateFor("")
	st.mu.Lock()
	st.state = circuitOpen
	cb.stateFor("").lastFailure = time.Now().Add(-cbOpenDuration - time.Second)
	cb.stateFor("").mu.Unlock()

	// The probe call panics; recover it here (as the real stream goroutine does).
	func() {
		defer func() {
			if r := recover(); r == nil {
				t.Fatal("expected the provider panic to propagate")
			}
		}()
		_, _ = cb.Chat(context.Background(), Request{})
	}()

	// The circuit must have been resolved (reopened), NOT left half-open.
	cb.stateFor("").mu.Lock()
	state := cb.stateFor("").state
	cb.stateFor("").mu.Unlock()
	if state == circuitHalfOpen {
		t.Fatal("circuit wedged half-open after a probe panic — record was skipped")
	}
	if state != circuitOpen {
		t.Fatalf("expected circuit reopened after probe panic, got %s", state)
	}

	// And it must recover normally: after the cooldown, a healthy probe closes it.
	// Reuse the SAME shared state (the shared state) with a healthy provider, exactly as the
	// next admitted probe would.
	cb.stateFor("").mu.Lock()
	cb.stateFor("").lastFailure = time.Now().Add(-cbOpenDuration - time.Second)
	cb.stateFor("").mu.Unlock()
	cb2 := &CircuitBreakerProvider{Provider: okStub{}, providerID: cb.providerID, failureThreshold: cbFailureThreshold, openDuration: cbOpenDuration}
	if _, err := cb2.Chat(context.Background(), Request{}); err != nil {
		t.Fatalf("post-cooldown probe on a healthy provider should succeed, got %v", err)
	}
	cb.stateFor("").mu.Lock()
	defer cb.stateFor("").mu.Unlock()
	if st := cb.stateFor(""); st.state != circuitClosed {
		t.Fatalf("expected circuit closed after a successful probe, got %s", st.state)
	}
}

// okStub always succeeds; used to close the circuit after recovery.
type okStub struct{ Provider }

func (okStub) Chat(context.Context, Request) (*Response, error) { return &Response{Content: "ok"}, nil }
func (okStub) ID() string                                       { return "panic-stub" }

// TestCircuitBreaker_ModelScopedIsolation pins the per-(provider|model) keying:
// a deprecated model ID 500-ing five times must open ONLY that model's
// circuit — its healthy siblings (and the provider-wide Embed path) keep
// working. The pre-fix provider-wide key blocked every model and every user
// for the cooldown.
func TestCircuitBreaker_ModelScopedIsolation(t *testing.T) {
	resetBreakerRegistry()
	cb := NewCircuitBreakerProvider(cbFailStub{})

	chat := func(model string) error {
		_, err := cb.Chat(context.Background(), Request{Model: model})
		return err
	}

	// Trip the bad model's circuit.
	for range cbFailureThreshold {
		if err := chat("deprecated-model"); !errors.Is(err, ErrProviderDown) {
			t.Fatalf("expected provider-down, got %v", err)
		}
	}
	if err := chat("deprecated-model"); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("bad model's circuit should be open, got %v", err)
	}

	// Healthy sibling: its circuit is closed — the call goes THROUGH to the
	// (failing) stub rather than being short-circuited by a shared breaker.
	if err := chat("healthy-model"); !errors.Is(err, ErrProviderDown) {
		t.Fatalf("healthy model blocked by the bad model's circuit: %v", err)
	}
	// Model-less requests (provider-wide key): unaffected.
	if _, err := cb.Embed(context.Background(), []string{"x"}); err != nil {
		if errors.Is(err, ErrCircuitOpen) {
			t.Fatal("provider-wide circuit opened by a single model's failures")
		}
	}
	// Distinct registry entries per model.
	breakerRegistryMu.Lock()
	defer breakerRegistryMu.Unlock()
	if _, ok := breakerRegistry["cbfail|deprecated-model"]; !ok {
		t.Errorf("model-scoped key missing from registry: %v", breakerRegistry)
	}
}

// cbFailStub fails every Chat; the embedded interface covers the rest of
// Provider (its Embed succeeds — the provider-wide circuit must stay closed).
type cbFailStub struct{ Provider }

func (cbFailStub) ID() string   { return "cbfail" }
func (cbFailStub) Name() string { return "fail" }
func (cbFailStub) Chat(context.Context, Request) (*Response, error) {
	return nil, ErrProviderDown
}
func (cbFailStub) Embed(context.Context, []string) ([][]float32, error) { return nil, nil }
