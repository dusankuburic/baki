package metrics

import (
	"maps"
	"regexp"
	"strings"
	"testing"

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
// set means a caller forgot to normalise it. The sets of the metrics other
// packages own are checked beside their recorders (the HTTP method label in
// internal/api/middleware, the CI webhook labels in internal/service).
var closedLabelValues = map[string]map[string]struct{}{
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

// constLabelName matches the names in Desc.String's constLabels list (name="value",…).
var constLabelName = regexp.MustCompile(`(?:^|,)([a-zA-Z_][a-zA-Z0-9_]*)="`)

// TestMetrics_NoUnboundedLabelNames reads the label names off the descriptor of every registered family, so it also
// covers a metric nothing has recorded yet, which the sample walk below cannot see. This test binary links every
// package that registers (TestRegistration_OnlyThroughThePackageRegistry), so these are all the families /metrics
// serves.
func TestMetrics_NoUnboundedLabelNames(t *testing.T) {
	descs := 0
	for _, reg := range registry.registrations() {
		for _, d := range describe(reg.collector) {
			descs++
			name, _, _ := parseDesc(t, d)
			labels := variableLabels(t, d)
			if m := descPattern.FindStringSubmatch(d.String()); m != nil {
				for _, c := range constLabelName.FindAllStringSubmatch(m[3], -1) {
					labels = append(labels, c[1])
				}
			}
			for _, l := range labels {
				if why, bad := unboundedLabels[strings.ToLower(l)]; bad {
					t.Errorf("metric %q declares unbounded label %q: %s", name, l, why)
				}
			}
		}
	}
	if descs < len(onBehalf) {
		t.Errorf("read %d descriptors, fewer than the central list holds: the walk is broken", descs)
	}
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
// already touched; the label names of every family, touched or not, are read off
// the descriptors by TestMetrics_NoUnboundedLabelNames above. The guard for the
// regression that started this (a per-tenant label on the AI cost counter) is
// TestAIUsageCost_GlobalAndUnlabelled in internal/ai, beside the recorder.
func TestMetrics_NoUnboundedLabels(t *testing.T) {
	RecordBackgroundDropped("cardinality-probe", "saturated")

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
