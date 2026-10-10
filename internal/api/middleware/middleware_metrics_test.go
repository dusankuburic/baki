package middleware

import (
	"testing"

	"pad-analyzer/internal/metrics"
)

// TestRoutePolicyCountersRegistered: both route-policy counters reach the
// registry under their documented names (a soak watches the first, any
// increase of the second is a resolver bug).
func TestRoutePolicyCountersRegistered(t *testing.T) {
	before, beforeGuard := RoutePolicyMismatchCount("/api/probe", "tx"), RouteGuardMismatchCount("/api/probe")
	recordRoutePolicyMismatch("/api/probe", "tx")
	recordRouteGuardMismatch("/api/probe")
	if got := RoutePolicyMismatchCount("/api/probe", "tx") - before; got != 1 {
		t.Errorf("mismatch count = %v, want 1", got)
	}
	if got := RouteGuardMismatchCount("/api/probe") - beforeGuard; got != 1 {
		t.Errorf("guard mismatch count = %v, want 1", got)
	}
	families, err := metrics.Gather()
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{"pad_route_policy_mismatch_total": false, "pad_route_policy_guard_mismatch_total": false}
	for _, mf := range families {
		if _, ok := want[mf.GetName()]; ok {
			want[mf.GetName()] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("%s was not gathered", name)
		}
	}
}
