package ai

import (
	"context"
	"strings"
	"testing"

	"pad-analyzer/internal/metrics"
)

// retryMetricStub fails the first n calls with a retryable error, then succeeds.
type retryMetricStub struct {
	Provider
	calls    int
	failures int
	err      error
}

func (s *retryMetricStub) ID() string { return "stubprov" }
func (s *retryMetricStub) Chat(context.Context, Request) (*Response, error) {
	s.calls++
	if s.calls <= s.failures {
		return nil, s.err
	}
	return &Response{Content: "ok"}, nil
}

// TestRetryAttempts_AreObservable is the operability gate on the retry layer.
//
// Retries were entirely invisible — no counter, no aggregate. An operator could
// not see a retry storm, could not explain latency inflated by backoff, and had
// no signal that a provider was degraded short of user complaints. The retry
// ceiling (PAD_RETRY_MAX_ATTEMPTS) has no upper bound, which makes the blind
// spot worse: a misconfiguration multiplies every call silently.
//
// Note what this does NOT claim. An earlier audit described the decorator order
// as a billing defect — "each retry costs a full prompt upstream and only the
// last is recorded". That is not established: the loop retries ONLY on
// ErrRateLimited (429) and ErrProviderDown (5xx), and in both cases the provider
// rejected the request rather than processing the prompt, so there is nothing to
// bill. Moving the audit wrapper inside the retry also would not have fixed it —
// auditedProvider records from the RESPONSE, and a failed attempt returns none.
// The real gap is visibility, which is what this covers.
func TestRetryAttempts_AreObservable(t *testing.T) {
	for _, tc := range []struct {
		name       string
		err        error
		wantReason string
	}{
		{"rate limited", ErrRateLimited, "rate_limited"},
		{"provider down", ErrProviderDown, "provider_down"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := countRetryMetric(t, tc.wantReason)

			stub := &retryMetricStub{failures: 2, err: tc.err}
			rp := NewRetryingProviderWithConfig(stub, 5, 0)
			if _, err := rp.Chat(context.Background(), Request{}); err != nil {
				t.Fatalf("chat: %v", err)
			}
			if stub.calls != 3 {
				t.Fatalf("fixture rot: provider saw %d attempts, expected 3", stub.calls)
			}

			after := countRetryMetric(t, tc.wantReason)
			// Three attempts = two RETRIES.
			if after-before != 2 {
				t.Errorf("recorded %d retries for %d attempts — a retry storm would be invisible "+
					"to every dashboard and alert", after-before, stub.calls)
			}
		})
	}
}

// countRetryMetric reads the counter out of the real registry, so the test
// exercises the wiring rather than a local variable.
func countRetryMetric(t *testing.T, reason string) int {
	t.Helper()
	fams, err := metrics.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() != "pad_ai_retry_attempts_total" {
			continue
		}
		for _, m := range f.GetMetric() {
			var prov, rsn string
			for _, l := range m.GetLabel() {
				switch l.GetName() {
				case "provider":
					prov = l.GetValue()
				case "reason":
					rsn = l.GetValue()
				}
			}
			if prov == "stubprov" && rsn == reason {
				return int(m.GetCounter().GetValue())
			}
		}
	}
	return 0
}

// TestRetryReason_IsAClosedSet guards the label against becoming unbounded.
// Raw error text would carry provider-specific detail and mint a permanent time
// series per distinct message — the cardinality bomb this repo has already had
// to fix twice.
func TestRetryReason_IsAClosedSet(t *testing.T) {
	allowed := map[string]bool{"rate_limited": true, "provider_down": true, "other": true}
	for _, err := range []error{ErrRateLimited, ErrProviderDown, context.Canceled, nil} {
		got := retryReason(err)
		if !allowed[got] {
			t.Errorf("retryReason(%v) = %q, which is outside the closed label set", err, got)
		}
		if strings.ContainsAny(got, " :/\"") {
			t.Errorf("retryReason(%v) = %q looks like raw error text", err, got)
		}
	}
}
