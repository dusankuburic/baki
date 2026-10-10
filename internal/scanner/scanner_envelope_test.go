package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"pad-core/models"

	"pad-analyzer/internal/notify"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// orgHook records the raw JSON bodies an org webhook channel receives.
type orgHook struct {
	mu     sync.Mutex
	bodies []map[string]any
	header []http.Header
	srv    *httptest.Server
}

func newOrgHook(t *testing.T) *orgHook {
	t.Helper()
	h := &orgHook{}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(raw, &body)
		h.mu.Lock()
		h.bodies = append(h.bodies, body)
		h.header = append(h.header, r.Header.Clone())
		h.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(h.srv.Close)
	return h
}

func (h *orgHook) received() []map[string]any {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]map[string]any(nil), h.bodies...)
}

// TestScanOnce_OrgChannelEventsCarryTheirTime is the regression gate for B3: the sweep built its events without a
// time and only the deployment dispatcher stamped its own copy, so every org channel received
// "at":"0001-01-01T00:00:00Z" (Slack's ts -62135596800, the email's "Time: 0001-01-01…").
func TestScanOnce_OrgChannelEventsCarryTheirTime(t *testing.T) {
	b, reports := alertingFlows(t, 1)
	hook := newOrgHook(t)
	if err := b.SaveOrgChannel(context.Background(), &storageif.OrgChannel{
		ID: "ch1", OrgID: "org-1", Name: "ops", Kind: "webhook", URL: hook.srv.URL, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	before := time.Now().Add(-time.Second)
	s := New(b, nil, analyzeReturning(reports), nil, 0, loopbackChannels(b))
	s.ScanOnce(context.Background())
	s.Stop()

	got := hook.received()
	if len(got) != 1 {
		t.Fatalf("org channel received %d events, want 1", len(got))
	}
	at, err := time.Parse(time.RFC3339Nano, got[0]["at"].(string))
	if err != nil || at.Before(before) {
		t.Errorf("org channel event at = %v (%v); want the time the sweep raised it", got[0]["at"], err)
	}
}

// TestScanOnce_OrgChannelEventCarriesTheEnvelope: an org channel's webhook body carries the schema version, the
// inbox row's event id (the same on every channel, retry and replica), the delivery id of its header and the org.
func TestScanOnce_OrgChannelEventCarriesTheEnvelope(t *testing.T) {
	b, reports := alertingFlows(t, 1)
	hook := newOrgHook(t)
	if err := b.SaveOrgChannel(context.Background(), &storageif.OrgChannel{
		ID: "ch1", OrgID: "org-1", Name: "ops", Kind: "webhook", URL: hook.srv.URL, Secret: "s3cret", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	s := New(b, nil, analyzeReturning(reports), nil, 0, loopbackChannels(b))
	s.ScanOnce(context.Background())
	s.Stop()

	got := hook.received()
	if len(got) != 1 {
		t.Fatalf("org channel received %d events, want 1", len(got))
	}
	body, header := got[0], hook.header[0]
	alerts, _ := b.ListGovernanceAlerts(context.Background(), storageif.GovernanceAlertFilter{})
	if len(alerts) != 1 {
		t.Fatalf("inbox alerts = %d, want 1", len(alerts))
	}
	if body["schemaVersion"] != float64(notify.SchemaVersion) || body["id"] != notify.EventID(alerts[0].ID) ||
		body["orgId"] != "org-1" || body["deliveryId"] != header.Get(notify.HeaderDelivery) || body["type"] != "drift" {
		t.Errorf("body %v, delivery header %q", body, header.Get(notify.HeaderDelivery))
	}
	if header.Get(notify.HeaderSignatureV2) == "" || header.Get(notify.HeaderSignature) == "" {
		t.Errorf("a signed channel's delivery lacks a signature: %v", header)
	}
}

// TestScanOnce_NewRulesNoticeReachesOrgChannelsOnce: ADR 0016 promised the notices to the channels in Phase 4c. A
// notice the sweep inserted goes to its organisation's channels once, at the sweep's last flush, with the counts of
// the whole sweep (the first page inserts it, the second grows it); no later sweep, and no restart, sends it again,
// and a user's notice (personal flows) reaches no channel.
func TestScanOnce_NewRulesNoticeReachesOrgChannelsOnce(t *testing.T) {
	b := newBackend()
	tenants(t, b, []string{"owner", "solo"}, map[string][]string{"org-1": {"owner"}})
	old := ruleSetOf(t, "unhandled-error")
	current := ruleSetOf(t, "unhandled-error", "hardcoded-url", "hardcoded-credential")
	reports := map[string]*models.AnalysisReport{}
	const flows = 130 // two pages
	for i := range flows {
		id := fmt.Sprintf("flow-%03d", i)
		seedOwnedFlow(t, b, id, "owner", "org-1")
		setBaselineWith(t, b, id, old)
		rule := "hardcoded-url"
		if i >= 100 {
			rule = "hardcoded-credential" // only the second page reports it
		}
		reports[id] = reportUnder(id, current, finding(rule, "b1", models.SeverityWarning))
	}
	seedOwnedFlow(t, b, "personal", "solo", "")
	setBaselineWith(t, b, "personal", old)
	reports["personal"] = reportUnder("personal", current, finding("hardcoded-url", "b1", models.SeverityWarning))
	hook := newOrgHook(t)
	if err := b.SaveOrgChannel(context.Background(), &storageif.OrgChannel{
		ID: "ch1", OrgID: "org-1", Name: "ops", Kind: "webhook", URL: hook.srv.URL, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	global := newCapture(t)

	for range 2 { // a sweep, then a restart
		s := New(b, nil, analyzeReturning(reports), mustNotifier(t, global.srv.URL), 0, loopbackChannels(b))
		s.ScanOnce(context.Background())
		s.ScanOnce(context.Background())
		s.Stop()
	}

	got := hook.received()
	if len(got) != 1 {
		t.Fatalf("org channel received %d deliveries, want the notice once: %v", len(got), got)
	}
	n := got[0]
	noticeID := storageif.RuleNoticeID("org", "org-1", current.Digest)
	rules, _ := n["ruleIds"].([]any)
	if n["type"] != string(notify.EventNewRules) || n["id"] != notify.EventID(noticeID) || n["flowCount"] != float64(flows) ||
		len(rules) != 2 || rules[0] != "hardcoded-credential" || rules[1] != "hardcoded-url" || n["orgId"] != "org-1" {
		t.Errorf("notice delivery = %v, want both rules in %d flows", n, flows)
	}
	if !strings.Contains(n["message"].(string), "hardcoded-credential, hardcoded-url") {
		t.Errorf("message %q does not name the rules", n["message"])
	}
	if d := global.all(); len(d) != 0 {
		t.Errorf("the deployment's channels received %+v: an organisation's or a user's notice is not theirs", d)
	}
}

// TestScanOnce_DeploymentNoticeReachesDeploymentChannels: without accounts every flow is the deployment's, and so is
// its notice, which goes to the deployment's channels once.
func TestScanOnce_DeploymentNoticeReachesDeploymentChannels(t *testing.T) {
	b := newBackend()
	old := ruleSetOf(t, "unhandled-error")
	current := ruleSetOf(t, "unhandled-error", "hardcoded-url")
	seedOwnedFlow(t, b, "f1", "local", "")
	setBaselineWith(t, b, "f1", old)
	reports := map[string]*models.AnalysisReport{"f1": reportUnder("f1", current, finding("hardcoded-url", "b1", models.SeverityWarning))}
	global := newCapture(t)
	s := New(b, nil, analyzeReturning(reports), mustNotifier(t, global.srv.URL), 0, loopbackChannels(b))
	s.SetSingleTenant(true)
	s.ScanOnce(context.Background())
	s.ScanOnce(context.Background())
	s.Stop()
	got := global.all()
	if len(got) != 1 || got[0].Type != notify.EventNewRules || got[0].FlowCount != 1 || got[0].At.IsZero() {
		t.Errorf("deployment deliveries = %+v, want the notice once", got)
	}
}
