package ai

import (
	"context"
	"errors"
	"testing"
	"time"
)

// gatedStreamStub emits a keepalive, then blocks until released before
// finishing — a long half-open probe stream.
type gatedStreamStub struct {
	Provider
	release chan struct{}
}

func (s *gatedStreamStub) ID() string { return "stream-stub" }
func (s *gatedStreamStub) Stream(_ context.Context, _ Request, onChunk func(Chunk)) error {
	onChunk(Chunk{})
	<-s.release
	onChunk(Chunk{Text: "answer"})
	onChunk(Chunk{Done: true})
	return nil
}

// TestCircuitBreaker_StreamProbeResolvesOnFirstChunk (PERF-11): the half-open
// probe closes the circuit on its first non-error chunk, so other callers of
// a recovered resource are admitted while the probe's stream is still
// running, not after it ends.
func TestCircuitBreaker_StreamProbeResolvesOnFirstChunk(t *testing.T) {
	resetBreakerRegistry()
	t.Cleanup(resetBreakerRegistry)

	// Trip the circuit through the stream path.
	failing := &cbStreamStub{chunks: []Chunk{{Error: ErrProviderDown}}}
	for range cbFailureThreshold {
		_ = NewCircuitBreakerProvider(failing).Stream(context.Background(), Request{}, func(Chunk) {})
	}
	if err := NewCircuitBreakerProvider(failing).Stream(context.Background(), Request{}, func(Chunk) {}); !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("circuit should be open, got %v", err)
	}
	st := NewCircuitBreakerProvider(failing).stateFor("")
	st.mu.Lock()
	st.lastFailure = time.Now().Add(-cbOpenDuration - time.Millisecond)
	st.mu.Unlock()

	// The probe: answered (keepalive) but still streaming.
	probe := &gatedStreamStub{release: make(chan struct{})}
	firstChunk := make(chan struct{})
	probeDone := make(chan error, 1)
	go func() {
		once := false
		probeDone <- NewCircuitBreakerProvider(probe).Stream(context.Background(), Request{}, func(Chunk) {
			if !once {
				once = true
				close(firstChunk)
			}
		})
	}()
	select {
	case <-firstChunk:
	case <-time.After(2 * time.Second):
		t.Fatal("the probe never started")
	}

	// Another caller, while the probe is still in flight.
	healthy := &cbStreamStub{chunks: []Chunk{{Text: "ok"}, {Done: true}}}
	if err := NewCircuitBreakerProvider(healthy).Stream(context.Background(), Request{}, func(Chunk) {}); err != nil {
		t.Errorf("a caller during the answered probe must be admitted, got %v", err)
	}
	close(probe.release)
	if err := <-probeDone; err != nil {
		t.Errorf("probe: %v", err)
	}
	st.mu.Lock()
	state, failures := st.state, st.failures
	st.mu.Unlock()
	if state != circuitClosed || failures != 0 {
		t.Errorf("after the probe: state=%v failures=%d, want closed/0", state, failures)
	}

	// A closed circuit's first chunk does not reset its failure count:
	// streams that answer and then fail still accumulate to the threshold.
	resetBreakerRegistry()
	late := &cbStreamStub{chunks: []Chunk{{Text: "partial"}, {Error: ErrProviderDown}}}
	for range cbFailureThreshold {
		_ = NewCircuitBreakerProvider(late).Stream(context.Background(), Request{}, func(Chunk) {})
	}
	if err := NewCircuitBreakerProvider(late).Stream(context.Background(), Request{}, func(Chunk) {}); !errors.Is(err, ErrCircuitOpen) {
		t.Errorf("answered-then-failed streams must still trip the circuit, got %v", err)
	}
}

// TestCircuitBreaker_CallerOwnProbeFailureDoesNotRestartCooldown (AZSEC-2): a
// half-open probe that fails with the caller's own error (a bogus key, a
// filtered prompt, a cancel) releases the probe without restarting the
// cooldown, so the next caller is admitted at once — a user with a garbage
// key can no longer keep another user's circuit open by racing the probes.
// An upstream failure (deadline, provider down) still restarts it.
func TestCircuitBreaker_CallerOwnProbeFailureDoesNotRestartCooldown(t *testing.T) {
	for _, own := range []error{ErrApiKeyInvalid, ErrContentFiltered, ErrContextLimit, ErrToolsUnsupported, context.Canceled, ErrProviderConfigInvalid} {
		resetBreakerRegistry()
		errs := make([]error, 0, cbFailureThreshold+1)
		for range cbFailureThreshold {
			errs = append(errs, ErrProviderDown)
		}
		errs = append(errs, own) // the attacker's probe; the stub succeeds afterwards
		cb := NewCircuitBreakerProvider(newCBStub(errs...))
		for range cbFailureThreshold {
			cbCall(t, cb)
		}
		if !errors.Is(cbCall(t, cb), ErrCircuitOpen) {
			t.Fatalf("%v: circuit should be open after the threshold", own)
		}
		cb.stateFor("").mu.Lock()
		cb.stateFor("").lastFailure = time.Now().Add(-cbOpenDuration - time.Millisecond)
		cb.stateFor("").mu.Unlock()
		if err := cbCall(t, cb); !errors.Is(err, own) {
			t.Fatalf("%v: the probe must return the caller's own error, got %v", own, err)
		}
		// The victim's very next call is the new probe and recovers.
		if err := cbCall(t, cb); err != nil {
			t.Errorf("%v: the next caller must be admitted at once, got %v", own, err)
		}
	}
	resetBreakerRegistry()
	t.Cleanup(resetBreakerRegistry)

	if probeSaysNothing(ErrProviderDown) || probeSaysNothing(context.DeadlineExceeded) || probeSaysNothing(ErrRateLimited) || probeSaysNothing(errors.New("boom")) {
		t.Error("upstream failures, a deadline and unknown errors must still restart the cooldown")
	}
	if !probeSaysNothing(&permissionDeniedError{msg: "x"}) {
		t.Error("PermissionDenied (also ErrApiKeyInvalid) is the caller's own")
	}
}
