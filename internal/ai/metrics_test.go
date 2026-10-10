package ai

import (
	"net/http/httptest"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"pad-analyzer/internal/metrics"
)

// TestAIMetricsExposed records AI request metrics and verifies they appear in
// the Prometheus exposition output, confirming both registration and recording.
func TestAIMetricsExposed(t *testing.T) {
	recordAITokens("unit-test-provider", 11, 22)
	observeAIRequest("unit-test-provider", 0.5)
	recordAIError("unit-test-provider")

	rr := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()

	for _, want := range []string{
		`ai_tokens_total{direction="input",provider="unit-test-provider"} 11`,
		`ai_tokens_total{direction="output",provider="unit-test-provider"} 22`,
		`ai_request_errors_total{provider="unit-test-provider"} 1`,
		`ai_request_duration_seconds_count{provider="unit-test-provider"} 1`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("metrics output missing %q", want)
		}
	}
}

// TestRecordAITokens_SkipsZero ensures zero-valued directions don't create
// spurious series.
func TestRecordAITokens_SkipsZero(t *testing.T) {
	recordAITokens("zero-test-provider", 0, 0)

	rr := httptest.NewRecorder()
	metrics.Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	if strings.Contains(rr.Body.String(), `provider="zero-test-provider"`) {
		t.Error("zero token counts should not emit an ai_tokens_total series")
	}
}

// TestAIUsageCost_GlobalAndUnlabelled: the cost counter is one global series. A {user_id, org_id} label once made
// it one permanent series per tenant (aiUsageCostTotal's comment); the recorder is touched first, so the family is
// present to inspect rather than absent and therefore clean.
func TestAIUsageCost_GlobalAndUnlabelled(t *testing.T) {
	recordAIUsageCost(0.01)

	families, err := metrics.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var found bool
	for _, mf := range families {
		if mf.GetName() == "pad_ai_usage_estimated_cost_total" {
			found = true
			if got := mf.GetType(); got != dto.MetricType_COUNTER {
				t.Errorf("type = %v, want COUNTER", got)
			}
			for _, m := range mf.GetMetric() {
				if len(m.GetLabel()) != 0 {
					t.Errorf("expected an unlabelled global counter, got labels %v", m.GetLabel())
				}
			}
		}
	}
	if !found {
		t.Error("pad_ai_usage_estimated_cost_total was not gathered")
	}
}
