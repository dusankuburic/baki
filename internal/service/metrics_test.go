package service

import (
	"testing"

	"pad-analyzer/internal/metrics"
)

// TestCIWebhookMetrics_ClosedLabelSets: the method and delivery labels of the CI webhook counters take the code
// constants only, never the key id, the token or the delivery id of a call. The recorders are touched first, so the
// families are present to inspect rather than absent and therefore clean.
func TestCIWebhookMetrics_ClosedLabelSets(t *testing.T) {
	for _, m := range []string{CIAuthKeyID, CIAuthGlobal, CIAuthFlowOrg, CIAuthToken} {
		recordCIWebhookAuth(m)
	}
	for _, d := range []string{CIDeliveryNew, CIDeliveryReplayed, CIDeliveryMissing} {
		recordCIWebhookDelivery(d)
	}
	closed := map[string]map[string]bool{
		"method":   {CIAuthKeyID: true, CIAuthGlobal: true, CIAuthFlowOrg: true, CIAuthToken: true},
		"delivery": {CIDeliveryNew: true, CIDeliveryReplayed: true, CIDeliveryMissing: true},
	}
	families, err := metrics.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]int{}
	for _, mf := range families {
		if mf.GetName() != "pad_ci_webhook_auth_total" && mf.GetName() != "pad_ci_webhook_deliveries_total" {
			continue
		}
		for _, m := range mf.GetMetric() {
			for _, lp := range m.GetLabel() {
				seen[lp.GetName()]++
				allowed, ok := closed[lp.GetName()]
				if !ok {
					t.Errorf("%s carries label %q, want method or delivery only", mf.GetName(), lp.GetName())
				} else if !allowed[lp.GetValue()] {
					t.Errorf("%s label %s has value %q outside its closed set", mf.GetName(), lp.GetName(), lp.GetValue())
				}
			}
		}
	}
	if seen["method"] < 4 || seen["delivery"] < 3 {
		t.Errorf("gathered %d method and %d delivery series, want at least 4 and 3", seen["method"], seen["delivery"])
	}
}
