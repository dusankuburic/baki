package metrics

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// TestRecordBackgroundLoopTick_UpdatesBothMetrics guards H20: a single call
// must increment the tick counter AND set the last-tick gauge, so the
// "time() - last_tick > 2 * interval" alert works.
func TestRecordBackgroundLoopTick_UpdatesBothMetrics(t *testing.T) {
	RecordBackgroundLoopTick("test-loop")

	rr := httptest.NewRecorder()
	Handler().ServeHTTP(rr, httptest.NewRequest("GET", "/metrics", nil))
	body := rr.Body.String()

	if !strings.Contains(body, `pad_background_loop_tick_total{loop="test-loop"} 1`) {
		t.Errorf("missing or wrong tick counter in:\n%s", body)
	}
	if !strings.Contains(body, `pad_background_loop_last_tick_timestamp_seconds{loop="test-loop"}`) {
		t.Errorf("missing last-tick gauge in:\n%s", body)
	}
}
