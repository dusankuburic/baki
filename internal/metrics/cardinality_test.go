package metrics

import (
	"maps"
	"strings"
	"testing"

	dto "github.com/prometheus/client_model/go"

	"pad-core/cache"
)

// unboundedLabels are label names whose value space is driven by data rather
// than by code. Each distinct value is a permanent time series — counters are
// never deleted — so one of these on a hot metric grows without limit until the
// scrape times out and the target drops, taking EVERY metric from the process
// with it, including the ones you need during the incident that caused it.
var unboundedLabels = map[string]string{
	"user_id":  "one series per user, forever; also publishes the customer roster to anyone with scrape access",
	"org_id":   "one series per tenant, forever",
	"flow_id":  "one series per flow, forever",
	"email":    "PII in a metric label",
	"path":     "unbounded by request URL — use a normalised route instead",
	"url":      "unbounded by request URL",
	"error":    "unbounded by error text",
	"token":    "credential material in a metric label",
	"block_id": "one series per block, forever",
}

// closedLabelValues are labels that ARE allowed, but only over a fixed value
// set: the value is client-controlled at the source, so anything outside the
// set means a caller forgot to normalise it. net/http accepts any token as a
// request method, so a verbatim r.Method is one series per invented method.
var closedLabelValues = map[string]map[string]struct{}{
	"method": {
		"GET": {}, "HEAD": {}, "POST": {}, "PUT": {}, "PATCH": {}, "DELETE": {},
		"CONNECT": {}, "OPTIONS": {}, "TRACE": {}, "OTHER": {},
	},
	"result": {cacheResultHit: {}, cacheResultMiss: {}, cacheResultRejected: {}},
}

// closedLabelSets is closedLabelValues plus the sets fixed at init: the cache label takes the declared cache names
// only (cache.Declare runs at package level with a literal name), never a key.
func closedLabelSets() map[string]map[string]struct{} {
	out := maps.Clone(closedLabelValues)
	names := map[string]struct{}{}
	for _, s := range cache.Specs() {
		names[s.Name] = struct{}{}
	}
	out["cache"] = names
	return out
}

// TestMetrics_NoUnboundedLabels walks the gathered metrics and fails on a label
// whose cardinality the server does not control.
//
// SCOPE, stated plainly because it is narrower than the name suggests:
// registry.Gather() returns only metrics that have OBSERVED A SAMPLE. A
// CounterVec that nothing has incremented yet contributes no series and
// therefore no labels, so this walk cannot see it. I wrote this test without
// that realisation, restored the bad labels to check it bit, and it passed —
// the walk was vacuous for exactly the metric it was written for.
//
// So: this catches an unbounded label on a metric something in the process has
// already touched. The load-bearing guard for the specific regression is
// TestMetrics_RegisteredAndGatherable below, which touches the recorder first
// and then asserts the shape. A genuinely complete guard would walk Desc
// descriptors rather than samples; that needs an enumeration of every collector,
// which is its own way to go stale.
func TestMetrics_NoUnboundedLabels(t *testing.T) {
	// Touch the recorders most at risk so their series exist to be inspected.
	RecordAIUsageCost(0.01)
	HTTPRequestsTotal.WithLabelValues("GET", "/", "200").Inc()

	families, err := registry.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	if len(families) == 0 {
		t.Fatal("no metrics registered — this test would pass vacuously")
	}

	closedLabelValues := closedLabelSets()
	cacheLabels := 0
	for _, mf := range families {
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				name := strings.ToLower(lp.GetName())
				if name == "cache" {
					cacheLabels++
				}
				if why, bad := unboundedLabels[name]; bad {
					t.Errorf("metric %q carries unbounded label %q: %s",
						mf.GetName(), lp.GetName(), why)
				}
				if allowed, closed := closedLabelValues[name]; closed {
					if _, ok := allowed[lp.GetValue()]; !ok {
						t.Errorf("metric %q label %q has value %q outside its closed set",
							mf.GetName(), lp.GetName(), lp.GetValue())
					}
				}
			}
		}
	}
	if cacheLabels == 0 {
		t.Error("no cache series gathered (metricstest.cache is declared in this binary): the cache label went unchecked")
	}
}

// TestMetrics_RegisteredAndGatherable is the vacuity guard for the test above:
// a Gather that returns nothing would make the label walk pass trivially.
func TestMetrics_RegisteredAndGatherable(t *testing.T) {
	// Touch the metric whose labels caused this test to exist, so it is present
	// in the gathered output rather than absent-and-therefore-clean.
	RecordAIUsageCost(0.01)

	families, err := registry.Gather()
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
