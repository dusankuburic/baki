package service

import "testing"

// pad_chat_tool_results_total{tool} must be a closed set: a registered tool
// keeps its name, anything the model invents collapses to "other".
func TestMetricToolLabel_ClosedSet(t *testing.T) {
	if got := metricToolLabel("search_flow"); got != "search_flow" {
		t.Errorf("registered tool relabelled: %q", got)
	}
	for _, bogus := range []string{"delete_everything", "search_flow2", "", "SEARCH_FLOW"} {
		if got := metricToolLabel(bogus); got != "other" {
			t.Errorf("unregistered %q must map to other, got %q", bogus, got)
		}
	}
}
