package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAIMetricsExposed records AI request metrics and verifies they appear in
// the Prometheus exposition output, confirming both registration and recording.
func TestAIMetricsExposed(t *testing.T) {
	RecordAITokens("unit-test-provider", 11, 22)
	ObserveAIRequest("unit-test-provider", 0.5)
	RecordAIError("unit-test-provider")

	rr := httptest.NewRecorder()
	Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
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
	RecordAITokens("zero-test-provider", 0, 0)

	rr := httptest.NewRecorder()
	Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	if strings.Contains(rr.Body.String(), `provider="zero-test-provider"`) {
		t.Error("zero token counts should not emit an ai_tokens_total series")
	}
}
