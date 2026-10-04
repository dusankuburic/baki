package metrics

import (
	"database/sql"
	"testing"
	"time"
)

// TestObservePoolStats_ExportsWaitsAndClosures: the pool gauges showed only
// in-use/idle/open, so neither pool exhaustion (requests waiting for a
// connection) nor idle-cap churn (connections closed on return) was visible.
func TestObservePoolStats_ExportsWaitsAndClosures(t *testing.T) {
	observePoolStats(sql.DBStats{
		OpenConnections:   7,
		InUse:             5,
		Idle:              2,
		WaitCount:         42,
		WaitDuration:      1500 * time.Millisecond,
		MaxIdleClosed:     11,
		MaxIdleTimeClosed: 3,
		MaxLifetimeClosed: 2,
	})

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	gauges := map[string]float64{}
	closed := map[string]float64{}
	for _, mf := range families {
		switch mf.GetName() {
		case "postgres_pool_wait_count", "postgres_pool_wait_duration_seconds", "postgres_pool_open":
			gauges[mf.GetName()] = mf.GetMetric()[0].GetGauge().GetValue()
		case "postgres_pool_closed_connections":
			for _, m := range mf.GetMetric() {
				if len(m.GetLabel()) != 1 || m.GetLabel()[0].GetName() != "reason" {
					t.Fatalf("closed_connections labels = %v, want exactly {reason}", m.GetLabel())
				}
				closed[m.GetLabel()[0].GetValue()] = m.GetGauge().GetValue()
			}
		}
	}
	for name, want := range map[string]float64{
		"postgres_pool_open":                  7,
		"postgres_pool_wait_count":            42,
		"postgres_pool_wait_duration_seconds": 1.5,
	} {
		if got, ok := gauges[name]; !ok || got != want {
			t.Errorf("%s = %v (present %v), want %v", name, got, ok, want)
		}
	}
	// A fixed label set: exactly these three reasons, nothing data-driven.
	want := map[string]float64{"max_idle": 11, "max_idle_time": 3, "max_lifetime": 2}
	if len(closed) != len(want) {
		t.Errorf("closed reasons = %v, want %v", closed, want)
	}
	for reason, v := range want {
		if closed[reason] != v {
			t.Errorf("closed{reason=%q} = %v, want %v", reason, closed[reason], v)
		}
	}
}
