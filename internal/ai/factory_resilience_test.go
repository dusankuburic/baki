package ai

import (
	"testing"
	"time"
)

// TestProviderFactory_ResilienceIsAppliedToEveryChain: the circuit breaker and retry settings the factory is built
// with (parsed once from the configuration) wrap each chain, and an unset one takes the package default, as a nil
// policy and a ProviderFactory literal do.
func TestProviderFactory_ResilienceIsAppliedToEveryChain(t *testing.T) {
	chain := func(t *testing.T, f *ProviderFactory) (*RetryingProvider, *CircuitBreakerProvider) {
		t.Helper()
		p := f.wrapChain(stubProvider{}, "u1", "openai")
		ap, ok := p.(*auditedProvider)
		if !ok {
			t.Fatalf("chain head is %T", p)
		}
		tp, ok := ap.inner.(*TracedProvider)
		if !ok {
			t.Fatalf("under the audit wrapper: %T", ap.inner)
		}
		rp, ok := tp.Provider.(*RetryingProvider)
		if !ok {
			t.Fatalf("under the tracing wrapper: %T", tp.Provider)
		}
		cb, ok := rp.Provider.(*CircuitBreakerProvider)
		if !ok {
			t.Fatalf("under the retry wrapper: %T", rp.Provider)
		}
		return rp, cb
	}

	rp, cb := chain(t, NewProviderFactory(nil, nil, nil, &ResilienceConfig{Threshold: 7, OpenFor: 45 * time.Second, Attempts: 2, BaseDelay: time.Second}))
	if cb.failureThreshold != 7 || cb.openDuration != 45*time.Second || rp.maxAttempts != 2 || rp.baseDelay != time.Second {
		t.Errorf("configured chain: threshold %d, open %v, attempts %d, delay %v; want 7, 45s, 2, 1s", cb.failureThreshold, cb.openDuration, rp.maxAttempts, rp.baseDelay)
	}
	for name, f := range map[string]*ProviderFactory{
		"nil policy":    NewProviderFactory(nil, nil, nil, nil),
		"zero policy":   NewProviderFactory(nil, nil, nil, &ResilienceConfig{}),
		"factory value": {},
	} {
		rp, cb := chain(t, f)
		if cb.failureThreshold != cbFailureThreshold || cb.openDuration != cbOpenDuration || rp.maxAttempts != retryMaxAttempts || rp.baseDelay != retryBaseDelay {
			t.Errorf("%s: threshold %d, open %v, attempts %d, delay %v; want the package defaults", name, cb.failureThreshold, cb.openDuration, rp.maxAttempts, rp.baseDelay)
		}
	}
}
