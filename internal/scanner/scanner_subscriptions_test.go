package scanner

import (
	"context"
	"testing"

	storageif "pad-analyzer/internal/storage/interfaces"
)

// TestScanOnce_OrgChannelsReceiveWhatTheySubscribeTo: a drift alert reaches the organisation's channels that receive
// drift, the default set included, and not one subscribed to new-rule notices only; a channel whose stored values
// cannot be decrypted is skipped without keeping the others from their deliveries.
func TestScanOnce_OrgChannelsReceiveWhatTheySubscribeTo(t *testing.T) {
	b, reports := alertingFlows(t, 1)
	byDefault, chosen, notices, unreadable := newOrgHook(t), newOrgHook(t), newOrgHook(t), newOrgHook(t)
	for _, ch := range []*storageif.OrgChannel{
		{ID: "default", URL: byDefault.srv.URL},
		{ID: "drift", URL: chosen.srv.URL, Events: []string{"drift", "health_regression"}},
		{ID: "notices", URL: notices.srv.URL, Events: []string{"new_rules"}},
		{ID: "unreadable", URL: unreadable.srv.URL},
	} {
		ch.OrgID, ch.Name, ch.Kind, ch.Enabled = "org-1", ch.ID, "webhook", true
		if err := b.SaveOrgChannel(context.Background(), ch); err != nil {
			t.Fatal(err)
		}
	}
	b.OrgChannels["unreadable"].Unreadable = true
	s := New(b, nil, analyzeReturning(reports), nil, 0, loopbackChannels(b))
	s.ScanOnce(context.Background())
	s.Stop()

	for name, h := range map[string]*orgHook{"default": byDefault, "drift": chosen} {
		if got := h.received(); len(got) != 1 || got[0]["type"] != "drift" {
			t.Errorf("%s channel received %v, want the drift alert", name, got)
		}
	}
	if got := notices.received(); len(got) != 0 {
		t.Errorf("a channel subscribed to new_rules only received %v", got)
	}
	if got := unreadable.received(); len(got) != 0 {
		t.Errorf("an unreadable channel was delivered to: %v", got)
	}
}
