package ai

// R-observability-7: a circuit's transitions name its model (bounded) and its resource (hashed), an open-circuit
// gauge says how many are open, and ErrCircuitOpen says which model and for how long.

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

type scopedStub struct {
	cbFailStub
	id, scope string
}

func (s scopedStub) ID() string           { return s.id }
func (s scopedStub) breakerScope() string { return s.scope }

func TestCircuitBreaker_LabelsGaugeAndTypedOpenError(t *testing.T) {
	resetBreakerRegistry()
	t.Cleanup(resetBreakerRegistry)
	const res = "https://contoso.openai.azure.com"
	cb := NewCircuitBreakerProvider(scopedStub{id: AzureFoundryProviderID, scope: res})
	before := testutil.ToFloat64(circuitBreakerTransitionsTotal.WithLabelValues(AzureFoundryProviderID, "deployment", scopeHash(res), "open"))
	openNow := func() int { return openCircuitCounts()[AzureFoundryProviderID] }

	req := Request{Model: "my-gpt"}
	for range cbFailureThreshold {
		_, _ = cb.Chat(context.Background(), req)
	}
	if got := testutil.ToFloat64(circuitBreakerTransitionsTotal.WithLabelValues(AzureFoundryProviderID, "deployment", scopeHash(res), "open")); got != before+1 {
		t.Errorf("open transitions labelled {azure-foundry, deployment, %s} = %v, want %v", scopeHash(res), got, before+1)
	}
	if len(scopeHash(res)) != 8 || scopeHash(res) == res {
		t.Errorf("resource label %q, want an 8-character hash", scopeHash(res))
	}
	if got := openNow(); got != 1 {
		t.Errorf("open circuits = %v, want 1 while the circuit is open", got)
	}
	if strings.Contains(err0(cb, req), "deployment") {
		t.Error("the open-circuit error text names a metric label")
	}

	_, err := cb.Chat(context.Background(), req)
	if !errors.Is(err, ErrCircuitOpen) {
		t.Fatalf("err = %v, want ErrCircuitOpen", err)
	}
	model, wait, ok := CircuitOpenDetail(err)
	if !ok || model != "deployment" || wait <= 0 || wait > cbOpenDuration {
		t.Errorf("CircuitOpenDetail = %q, %v, %v; want deployment and the remaining cooldown", model, wait, ok)
	}

	// The probe succeeds: the circuit closes and the gauge comes back.
	st := cb.stateFor("my-gpt")
	st.mu.Lock()
	st.lastFailure = time.Now().Add(-cbOpenDuration - time.Millisecond)
	st.mu.Unlock()
	cb.Provider = okStub{}
	if _, err := cb.Chat(context.Background(), req); err != nil {
		t.Fatalf("probe: %v", err)
	}
	if got := openNow(); got != 0 {
		t.Errorf("open circuits after close = %v, want 0", got)
	}

	// A circuit nobody calls again stops counting once its cooldown has ended.
	for range cbFailureThreshold {
		cb.Provider = scopedStub{id: AzureFoundryProviderID, scope: res}
		_, _ = cb.Chat(context.Background(), Request{Model: "other"})
	}
	if openNow() != 1 {
		t.Fatalf("open circuits = %d, want 1", openNow())
	}
	other := cb.stateFor("other")
	other.mu.Lock()
	other.lastFailure = time.Now().Add(-cbOpenDuration - time.Second)
	other.mu.Unlock()
	if openNow() != 0 {
		t.Errorf("an abandoned circuit past its cooldown still counts as open")
	}
}

func err0(cb *CircuitBreakerProvider, req Request) string {
	_, err := cb.Chat(context.Background(), req)
	if err == nil {
		return ""
	}
	return err.Error()
}

func TestMetricModelLabel_IsBounded(t *testing.T) {
	catalogued := ""
	for _, m := range catalogModels("openai") {
		catalogued = m.ID
		break
	}
	for _, tc := range []struct{ provider, model, want string }{
		{"openai", catalogued, catalogued},
		{"openai", "my-fine-tune-123", "uncatalogued"},
		{"openai", strings.ToUpper(catalogued), catalogued},
		{AzureFoundryProviderID, "anything", "deployment"},
		{"openai", "", "none"},
	} {
		if tc.model == "" && tc.want == "" {
			continue
		}
		if got := metricModelLabel(tc.provider, tc.model); got != tc.want {
			t.Errorf("metricModelLabel(%q, %q) = %q, want %q", tc.provider, tc.model, got, tc.want)
		}
	}
}
