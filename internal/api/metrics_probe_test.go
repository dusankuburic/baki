package api

import (
	"testing"

	"pad-analyzer/internal/metrics"
)

// counterSum returns the sum of every sample of a counter family whose labels
// include all of match. Reads through the real registry (metrics.Gather) so a
// metric that is defined but never reached counts as zero.
func counterSum(t *testing.T, family string, match map[string]string) float64 {
	t.Helper()
	fams, err := metrics.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	var sum float64
	for _, f := range fams {
		if f.GetName() != family {
			continue
		}
		for _, m := range f.GetMetric() {
			ok := true
			for k, v := range match {
				found := false
				for _, lp := range m.GetLabel() {
					if lp.GetName() == k {
						found = lp.GetValue() == v
					}
				}
				if !found {
					ok = false
					break
				}
			}
			if ok {
				sum += m.GetCounter().GetValue()
			}
		}
	}
	return sum
}
