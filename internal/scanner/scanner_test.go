package scanner

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"pad-core/analyzer"
	"pad-core/models"

	"pad-analyzer/internal/egress"
	"pad-analyzer/internal/notify"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// capture is an httptest webhook that records the events the dispatcher delivers.
type capture struct {
	mu     sync.Mutex
	events []notify.Event
	srv    *httptest.Server
}

func newCapture(t *testing.T) *capture {
	t.Helper()
	c := &capture{}
	c.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var ev notify.Event
		if err := json.Unmarshal(body, &ev); err == nil {
			c.mu.Lock()
			c.events = append(c.events, ev)
			c.mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(c.srv.Close)
	return c
}

// mustNotifier builds a dispatcher for the (localhost httptest) webhook URL.
// notify.New now returns an error to reject plaintext remote URLs; test URLs
// are always valid http://127.0.0.1 so the error can't fire here.
func mustNotifier(t *testing.T, url string) *notify.Dispatcher {
	t.Helper()
	// The capture servers are httptest, i.e. loopback — the policy has to be
	// opted into explicitly, which is the point of the zero value being strict.
	d, err := notify.New(notify.Config{Policy: notify.AllowLoopbackTargets,
		Channels: []notify.ChannelConfig{{Kind: "webhook", Values: notify.Values{"url": url}}}})
	if err != nil {
		t.Fatalf("notify.New: %v", err)
	}
	return d
}

// loopbackChannels is the org-channel service over store with the policy of a deployment without accounts: the test
// receivers are loopback. A nil store is no org channels.
func loopbackChannels(store storageif.OrgChannelStore) OrgChannels {
	if store == nil {
		return nil
	}
	return service.NewChannelService(store, service.ChannelOptions{Policy: egress.AllowLoopback})
}

func (c *capture) all() []notify.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]notify.Event(nil), c.events...)
}

// healthBackend embeds the in-memory FakeBackend; health is the fake's own
// FlowHealth map (seed it to exercise the regression path), so both the
// single and the batched health reads — and their call counters — see it.
type healthBackend struct {
	*testutil.FakeBackend
	health map[string]*storageif.HealthSnapshot
}

func newBackend() *healthBackend {
	fb := testutil.NewFakeBackend()
	fb.FlowHealth = map[string]*storageif.HealthSnapshot{}
	return &healthBackend{FakeBackend: fb, health: fb.FlowHealth}
}

func seedFlow(t *testing.T, b storageif.StorageBackend, id string) {
	t.Helper()
	seedOrgFlow(t, b, id, "")
}

// seedOrgFlow seeds a flow in orgID; a flow's organisation is set when it is created (a later save keeps it).
func seedOrgFlow(t *testing.T, b storageif.StorageBackend, id, orgID string) {
	t.Helper()
	if err := b.SaveFlow(context.Background(), &storageif.FlowDocument{
		ID:             id,
		Name:           id,
		Content:        json.RawMessage(`{"id":"` + id + `","subflows":[]}`),
		OrganizationID: orgID,
	}); err != nil {
		t.Fatalf("seed flow %s: %v", id, err)
	}
}

func analyzeReturning(reports map[string]*models.AnalysisReport) AnalyzeFunc {
	return func(_ context.Context, doc *models.FlowDocument) (*models.AnalysisReport, error) {
		return reports[doc.ID], nil
	}
}

func TestScanOnce_DriftAlert(t *testing.T) {
	b := newBackend()
	seedFlow(t, b, "f1")
	// Baseline accepts r1:b1; the next analysis adds r2:b2 (a new error).
	if err := b.SetFlowBaseline(context.Background(), &storageif.FlowBaseline{FlowID: "f1", Keys: []string{"r1:b1"}}); err != nil {
		t.Fatalf("set baseline: %v", err)
	}
	reports := map[string]*models.AnalysisReport{
		"f1": {FlowID: "f1", Findings: []models.Finding{
			{RuleID: "r1", BlockID: "b1", Severity: models.SeverityWarning},
			{RuleID: "r2", BlockID: "b2", Severity: models.SeverityError},
		}},
	}

	cap := newCapture(t)
	s := New(b, nil, analyzeReturning(reports), mustNotifier(t, cap.srv.URL), 0, loopbackChannels(b))
	s.ScanOnce(context.Background())

	got := cap.all()
	if len(got) != 1 {
		t.Fatalf("expected 1 alert, got %d (%+v)", len(got), got)
	}
	if got[0].Type != notify.EventDrift || got[0].NewErrors != 1 {
		t.Errorf("unexpected drift event: %+v", got[0])
	}
}

func TestScanOnce_NoBaselineNoAlert(t *testing.T) {
	b := newBackend()
	seedFlow(t, b, "f1")
	reports := map[string]*models.AnalysisReport{
		"f1": {FlowID: "f1", Findings: []models.Finding{{RuleID: "r2", BlockID: "b2", Severity: models.SeverityError}}},
	}

	cap := newCapture(t)
	s := New(b, nil, analyzeReturning(reports), mustNotifier(t, cap.srv.URL), 0, loopbackChannels(b))
	s.ScanOnce(context.Background())

	if got := cap.all(); len(got) != 0 {
		t.Errorf("expected no alerts without a baseline, got %d (%+v)", len(got), got)
	}
}

func TestScanOnce_HealthRegression(t *testing.T) {
	b := newBackend()
	seedFlow(t, b, "f1")
	b.health["f1"] = &storageif.HealthSnapshot{HealthScore: 90}
	reports := map[string]*models.AnalysisReport{
		"f1": {FlowID: "f1", Metrics: &models.FlowMetrics{HealthScore: 70}},
	}

	cap := newCapture(t)
	s := New(b, nil, analyzeReturning(reports), mustNotifier(t, cap.srv.URL), 0, loopbackChannels(b))
	s.ScanOnce(context.Background())

	got := cap.all()
	if len(got) != 1 {
		t.Fatalf("expected 1 regression alert, got %d (%+v)", len(got), got)
	}
	if got[0].Type != notify.EventHealthRegression || got[0].PrevHealth != 90 || got[0].HealthScore != 70 {
		t.Errorf("unexpected regression event: %+v", got[0])
	}
}

func TestScanOnce_NoRegressionWhenHealthImproves(t *testing.T) {
	b := newBackend()
	seedFlow(t, b, "f1")
	b.health["f1"] = &storageif.HealthSnapshot{HealthScore: 70}
	reports := map[string]*models.AnalysisReport{
		"f1": {FlowID: "f1", Metrics: &models.FlowMetrics{HealthScore: 95}},
	}

	cap := newCapture(t)
	s := New(b, nil, analyzeReturning(reports), mustNotifier(t, cap.srv.URL), 0, loopbackChannels(b))
	s.ScanOnce(context.Background())

	if got := cap.all(); len(got) != 0 {
		t.Errorf("improving health must not alert, got %d (%+v)", len(got), got)
	}
}

func TestScanOnce_DedupsRepeatAlerts(t *testing.T) {
	b := newBackend()
	seedFlow(t, b, "f1")
	if err := b.SetFlowBaseline(context.Background(), &storageif.FlowBaseline{FlowID: "f1", Keys: []string{}}); err != nil {
		t.Fatalf("set baseline: %v", err)
	}
	reports := map[string]*models.AnalysisReport{
		"f1": {FlowID: "f1", Findings: []models.Finding{{RuleID: "r2", BlockID: "b2", Severity: models.SeverityError}}},
	}

	cap := newCapture(t)
	s := New(b, nil, analyzeReturning(reports), mustNotifier(t, cap.srv.URL), 0, loopbackChannels(b))

	s.ScanOnce(context.Background())
	s.ScanOnce(context.Background()) // identical state — must not re-alert

	if got := cap.all(); len(got) != 1 {
		t.Errorf("expected 1 alert across two identical scans (deduped), got %d", len(got))
	}
}

// TestScanOnce_PrunesDedupEntriesForDeletedFlows verifies lastSig doesn't
// accumulate forever: once a flow that triggered an alert is deleted, a
// subsequent complete sweep drops its dedup entry so re-creating a flow with
// the same ID doesn't inherit stale dedup state, and the map doesn't grow
// unbounded over the scanner's lifetime.
func TestScanOnce_PrunesDedupEntriesForDeletedFlows(t *testing.T) {
	b := newBackend()
	seedFlow(t, b, "f1")
	if err := b.SetFlowBaseline(context.Background(), &storageif.FlowBaseline{FlowID: "f1", Keys: []string{}}); err != nil {
		t.Fatalf("set baseline: %v", err)
	}
	reports := map[string]*models.AnalysisReport{
		"f1": {FlowID: "f1", Findings: []models.Finding{{RuleID: "r2", BlockID: "b2", Severity: models.SeverityError}}},
	}

	cap := newCapture(t)
	s := New(b, nil, analyzeReturning(reports), mustNotifier(t, cap.srv.URL), 0, loopbackChannels(b))

	s.ScanOnce(context.Background())
	s.mu.Lock()
	n := len(s.lastSig)
	s.mu.Unlock()
	if n == 0 {
		t.Fatal("expected lastSig to have an entry for f1 after its alert")
	}

	if err := b.DeleteFlow(context.Background(), "f1"); err != nil {
		t.Fatalf("delete flow: %v", err)
	}
	s.ScanOnce(context.Background()) // full sweep over an empty flow list

	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.lastSig) != 0 {
		t.Errorf("lastSig = %v, want empty after the deleted flow's entry is pruned", s.lastSig)
	}
}

// TestScanOnce_PersistsAlertToInbox verifies the scanner writes a governance
// event to the in-app alert store (the notifications bell's backing data) in
// addition to dispatching to the external webhook. The alert ID is derived from
// (flow|type|signature) so a re-alert for the same regression reuses the row.
func TestScanOnce_PersistsAlertToInbox(t *testing.T) {
	b := newBackend()
	seedFlow(t, b, "f1")
	if err := b.SetFlowBaseline(context.Background(), &storageif.FlowBaseline{FlowID: "f1", Keys: []string{}}); err != nil {
		t.Fatalf("set baseline: %v", err)
	}
	reports := map[string]*models.AnalysisReport{
		"f1": {FlowID: "f1", Findings: []models.Finding{{RuleID: "r2", BlockID: "b2", Severity: models.SeverityError}}},
	}

	cap := newCapture(t)
	s := New(b, nil, analyzeReturning(reports), mustNotifier(t, cap.srv.URL), 0, loopbackChannels(b))
	s.ScanOnce(context.Background())

	// One external dispatch AND one persisted inbox row.
	if got := cap.all(); len(got) != 1 {
		t.Fatalf("expected 1 webhook dispatch, got %d", len(got))
	}
	alerts, err := b.ListGovernanceAlerts(context.Background(), storageif.GovernanceAlertFilter{})
	if err != nil {
		t.Fatalf("ListGovernanceAlerts: %v", err)
	}
	if len(alerts) != 1 {
		t.Fatalf("expected 1 inbox alert, got %d", len(alerts))
	}
	a := alerts[0]
	if a.FlowID != "f1" || a.Type != "drift" || a.Severity != "error" || a.NewErrors != 1 {
		t.Errorf("unexpected persisted alert: %+v", a)
	}
	if a.ReadAt != nil {
		t.Errorf("fresh inbox alert should be unread, got readAt=%v", a.ReadAt)
	}

	// A second identical scan is de-duplicated upstream (shouldAlert), so no new
	// inbox row is written (the existing one is reused via ON CONFLICT DO NOTHING).
	s.ScanOnce(context.Background())
	alerts, _ = b.ListGovernanceAlerts(context.Background(), storageif.GovernanceAlertFilter{})
	if len(alerts) != 1 {
		t.Errorf("expected 1 inbox alert after a re-scan (deduped), got %d", len(alerts))
	}
}

// fakeSSE captures EmitTo calls so a test can assert the scanner pushed a
// real-time event. It implements scanner.SSENotifier.
type fakeSSE struct {
	mu      sync.Mutex
	pushed  []sseCall
	seenUID map[string]bool
}

type sseCall struct {
	userID string
	name   string
	data   any
}

func (f *fakeSSE) EmitTo(userID, name string, data any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.pushed = append(f.pushed, sseCall{userID, name, data})
	if f.seenUID == nil {
		f.seenUID = map[string]bool{}
	}
	f.seenUID[userID] = true
}

func (f *fakeSSE) calls() []sseCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]sseCall(nil), f.pushed...)
}

// TestScanOnce_PushesAlertOverSSE verifies the scanner emits a governance:alert
// SSE event to the flow owner (and org members / collaborators) so the bell
// updates in real time. The flow owner is the recipient exercised here.
func TestScanOnce_PushesAlertOverSSE(t *testing.T) {
	b := newBackend()
	// Seed the flow with an explicit owner so the SSE recipient is resolvable.
	if err := b.SaveFlow(context.Background(), &storageif.FlowDocument{
		ID: "f1", Name: "f1", OwnerID: "owner-1",
		Content: json.RawMessage(`{"id":"f1","subflows":[]}`),
	}); err != nil {
		t.Fatalf("seed flow: %v", err)
	}
	if err := b.SetFlowBaseline(context.Background(), &storageif.FlowBaseline{FlowID: "f1", Keys: []string{}}); err != nil {
		t.Fatalf("set baseline: %v", err)
	}
	reports := map[string]*models.AnalysisReport{
		"f1": {FlowID: "f1", Findings: []models.Finding{{RuleID: "r2", BlockID: "b2", Severity: models.SeverityError}}},
	}

	cap := newCapture(t)
	s := New(b, nil, analyzeReturning(reports), mustNotifier(t, cap.srv.URL), 0, loopbackChannels(b))
	sse := &fakeSSE{}
	s.SetEventNotifier(sse)
	s.ScanOnce(context.Background())

	calls := sse.calls()
	if len(calls) != 1 {
		t.Fatalf("expected 1 SSE push, got %d (%+v)", len(calls), calls)
	}
	if calls[0].name != "governance:alert" {
		t.Errorf("expected event name governance:alert, got %q", calls[0].name)
	}
	if calls[0].userID != "owner-1" {
		t.Errorf("expected push to owner-1, got %q", calls[0].userID)
	}
}

// TestScanOnce_OrgChannelRouting pins R2-3: a drift event for an org-scoped
// flow is delivered to the ORG's OWN channel in addition to the global one;
// a disabled org channel receives nothing; an unscoped flow routes only
// globally.
func TestScanOnce_OrgChannelRouting(t *testing.T) {
	b := newBackend()
	seedOrgFlow(t, b, "f-org", "org-1")
	seedFlow(t, b, "f-plain")

	for _, f := range []string{"f-org", "f-plain"} {
		if err := b.SetFlowBaseline(context.Background(), &storageif.FlowBaseline{FlowID: f, Keys: []string{"r1:b1"}}); err != nil {
			t.Fatalf("set baseline: %v", err)
		}
	}
	reports := map[string]*models.AnalysisReport{
		"f-org": {FlowID: "f-org", Findings: []models.Finding{
			{RuleID: "r1", BlockID: "b1", Severity: models.SeverityWarning},
			{RuleID: "r2", BlockID: "b2", Severity: models.SeverityError},
		}},
		"f-plain": {FlowID: "f-plain", Findings: []models.Finding{
			{RuleID: "r1", BlockID: "b1", Severity: models.SeverityWarning},
			{RuleID: "r3", BlockID: "b3", Severity: models.SeverityError},
		}},
	}

	global := newCapture(t)
	orgHook := newCapture(t)
	orgHookDisabled := newCapture(t)
	if err := b.SaveOrgChannel(context.Background(), &storageif.OrgChannel{
		ID: "ch1", OrgID: "org-1", Name: "ops", Kind: "webhook", URL: orgHook.srv.URL, Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	if err := b.SaveOrgChannel(context.Background(), &storageif.OrgChannel{
		ID: "ch2", OrgID: "org-1", Name: "off", Kind: "webhook", URL: orgHookDisabled.srv.URL, Enabled: false,
	}); err != nil {
		t.Fatal(err)
	}

	s := New(b, nil, analyzeReturning(reports), mustNotifier(t, global.srv.URL), 0, loopbackChannels(b))
	s.ScanOnce(context.Background())

	// Org delivery runs on detached goroutines (parity with the global
	// dispatcher) — poll briefly for the async hit before asserting.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && len(orgHook.all()) < 1 {
		time.Sleep(10 * time.Millisecond)
	}

	// Global dispatcher: both flows' drifts.
	if got := global.all(); len(got) != 2 {
		t.Fatalf("global channel: %d events, want 2", len(got))
	}
	// Org channel: ONLY the org flow's drift (not the plain flow's).
	got := orgHook.all()
	if len(got) != 1 {
		t.Fatalf("org channel: %d events, want 1", len(got))
	}
	if got[0].FlowID != "f-org" || got[0].NewErrors != 1 {
		t.Errorf("org channel event wrong: %+v", got[0])
	}
	// Disabled channel: silent.
	if n := len(orgHookDisabled.all()); n != 0 {
		t.Fatalf("disabled org channel got %d events, want 0", n)
	}
}

// TestEnabled_DoesNotRequireEnvNotifier pins the gate that decides whether the
// sweep runs at all. It used to demand a deployment-level PAD_NOTIFY_* channel,
// which left every org that configured channels only through the UI (and the
// in-app governance inbox) silently without alerts.
func TestEnabled_DoesNotRequireEnvNotifier(t *testing.T) {
	b := testutil.NewFakeBackend()
	empty, err := notify.New(notify.Config{Policy: notify.AllowLoopbackTargets})
	if err != nil {
		t.Fatalf("notify.New: %v", err)
	}
	if empty.Enabled() {
		t.Fatalf("precondition: a dispatcher with no channels must report disabled")
	}
	cases := map[string]*notify.Dispatcher{"empty dispatcher": empty, "nil dispatcher": nil}
	for name, d := range cases {
		s := New(b, nil, analyzeReturning(nil), d, time.Hour, loopbackChannels(b))
		if !s.enabled() {
			t.Errorf("%s: scanner must be enabled with a backend, analyzer and interval", name)
		}
	}
	if s := New(b, nil, analyzeReturning(nil), empty, 0, loopbackChannels(b)); s.enabled() {
		t.Errorf("zero interval must still disable the scanner")
	}
	if s := New(nil, nil, analyzeReturning(nil), empty, time.Hour, loopbackChannels(nil)); s.enabled() {
		t.Errorf("nil backend must still disable the scanner")
	}
}

// skipHarness is a scanner with skipping enabled (a rule-set digest wired),
// an analyzer that counts runs per flow, and a mutable rule-set digest.
type skipHarness struct {
	b   *healthBackend
	s   *Scanner
	cap *capture

	mu       sync.Mutex
	runs     map[string]int
	reports  map[string]*models.AnalysisReport
	fail     map[string]bool
	ruleHash string
}

func newSkipHarness(t *testing.T, b *healthBackend) *skipHarness {
	h := &skipHarness{
		b:        b,
		cap:      newCapture(t),
		runs:     map[string]int{},
		reports:  map[string]*models.AnalysisReport{},
		fail:     map[string]bool{},
		ruleHash: "rules-v1",
	}
	analyze := func(_ context.Context, doc *models.FlowDocument) (*models.AnalysisReport, error) {
		h.mu.Lock()
		defer h.mu.Unlock()
		h.runs[doc.ID]++
		if h.fail[doc.ID] {
			return nil, errors.New("analysis failed")
		}
		if r, ok := h.reports[doc.ID]; ok {
			return r, nil
		}
		return &models.AnalysisReport{FlowID: doc.ID}, nil
	}
	h.s = New(b, nil, analyze, mustNotifier(t, h.cap.srv.URL), 0, loopbackChannels(b))
	h.s.SetRuleSetDigest(func(context.Context, string) string {
		h.mu.Lock()
		defer h.mu.Unlock()
		return h.ruleHash
	})
	return h
}

func (h *skipHarness) runsOf(flowID string) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.runs[flowID]
}

// TestScanOnce_SkipsFlowsWhoseInputsAreUnchanged: every sweep used to parse
// and analyze every flow, and with the analysis cache capped at 50 reports a
// sweep over a larger library missed on all of them (evicting interactive
// users' entries on the way). A flow nothing has changed for is analyzed once.
func TestScanOnce_SkipsFlowsWhoseInputsAreUnchanged(t *testing.T) {
	b := newBackend()
	seedFlow(t, b, "f1")
	seedFlow(t, b, "f2")
	h := newSkipHarness(t, b)

	for range 3 {
		h.s.ScanOnce(context.Background())
	}
	if f1, f2 := h.runsOf("f1"), h.runsOf("f2"); f1 != 1 || f2 != 1 {
		t.Fatalf("three sweeps over unchanged flows analyzed f1 %d and f2 %d times, want 1 each", f1, f2)
	}
}

// TestScanOnce_RescansAFlowWhenAnInputChanges walks every input the skip key
// covers: changing any one of them re-analyzes that flow (and, except for the
// shared rule set, only that flow), and a changed baseline or recorded health
// still raises its alert.
func TestScanOnce_RescansAFlowWhenAnInputChanges(t *testing.T) {
	ctx := context.Background()
	cases := []struct {
		name   string
		change func(t *testing.T, h *skipHarness)
		alert  notify.EventType // "" = the change raises no alert
	}{
		{"content", func(t *testing.T, h *skipHarness) {
			if err := h.b.SaveFlow(ctx, &storageif.FlowDocument{ID: "f1", Name: "f1",
				Content: json.RawMessage(`{"id":"f2","subflows":[]}`)}); err != nil { // same length, new bytes
				t.Fatal(err)
			}
		}, ""},
		// A save keeps a flow's organisation and owner, so these two change the stored row (the version stays).
		{"organization", func(t *testing.T, h *skipHarness) {
			h.b.Flows["f1"].OrganizationID = "org-2"
		}, ""},
		{"owner", func(t *testing.T, h *skipHarness) {
			h.b.Flows["f1"].OwnerID = "u-2"
		}, ""},
		{"rule set", func(t *testing.T, h *skipHarness) {
			h.mu.Lock()
			h.ruleHash = "rules-v2"
			h.mu.Unlock()
		}, ""},
		{"baseline keys", func(t *testing.T, h *skipHarness) {
			// Same number of keys; r2:b2 is no longer accepted.
			if err := h.b.SetFlowBaseline(ctx, &storageif.FlowBaseline{FlowID: "f1", Keys: []string{"r1:b1", "r3:b3"}}); err != nil {
				t.Fatal(err)
			}
		}, notify.EventDrift},
		{"baseline removed", func(t *testing.T, h *skipHarness) {
			delete(h.b.Baselines, "f1")
		}, ""},
		{"baseline rule set", func(t *testing.T, h *skipHarness) {
			// Re-baselined with the same keys, now recording the rule set it was taken under.
			if err := h.b.SetFlowBaseline(ctx, &storageif.FlowBaseline{FlowID: "f1", Keys: []string{"r1:b1", "r2:b2"},
				RuleSet: analyzer.NewRuleSet([]models.RuleRef{{ID: "r1"}, {ID: "r2"}})}); err != nil {
				t.Fatal(err)
			}
		}, ""},
		{"recorded health rule set", func(t *testing.T, h *skipHarness) {
			h.b.health["f1"] = &storageif.HealthSnapshot{HealthScore: 60, RuleSetDigest: "rs1:recorded"}
		}, ""},
		{"recorded health", func(t *testing.T, h *skipHarness) {
			h.b.health["f1"] = &storageif.HealthSnapshot{HealthScore: 90}
		}, notify.EventHealthRegression},
		{"recorded health removed", func(t *testing.T, h *skipHarness) {
			delete(h.b.health, "f1")
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			b := newBackend()
			seedFlow(t, b, "f1")
			seedFlow(t, b, "f2")
			// f1's report matches its baseline (no drift) and is healthier
			// than its recorded health (no regression): no alert to start.
			if err := b.SetFlowBaseline(ctx, &storageif.FlowBaseline{FlowID: "f1", Keys: []string{"r1:b1", "r2:b2"}}); err != nil {
				t.Fatal(err)
			}
			b.health["f1"] = &storageif.HealthSnapshot{HealthScore: 60}
			h := newSkipHarness(t, b)
			h.reports["f1"] = &models.AnalysisReport{FlowID: "f1",
				Findings: []models.Finding{
					{RuleID: "r1", BlockID: "b1", Severity: models.SeverityWarning},
					{RuleID: "r2", BlockID: "b2", Severity: models.SeverityError},
				},
				Metrics: &models.FlowMetrics{HealthScore: 70},
			}

			h.s.ScanOnce(ctx)
			h.s.ScanOnce(ctx)
			if n := h.runsOf("f1"); n != 1 {
				t.Fatalf("f1 analyzed %d times before the change, want 1", n)
			}

			tc.change(t, h)
			h.s.ScanOnce(ctx)
			if n := h.runsOf("f1"); n != 2 {
				t.Fatalf("f1 analyzed %d times after its %s changed, want 2", n, tc.name)
			}
			if n := h.runsOf("f2"); n != 1 && tc.name != "rule set" {
				t.Fatalf("unchanged f2 was re-analyzed (%d runs)", n)
			}

			got := h.cap.all()
			switch {
			case tc.alert == "" && len(got) != 0:
				t.Fatalf("unexpected alerts: %+v", got)
			case tc.alert != "" && (len(got) != 1 || got[0].Type != tc.alert):
				t.Fatalf("alerts = %+v, want one %s", got, tc.alert)
			}

			// And the new inputs are now the recorded ones.
			h.s.ScanOnce(ctx)
			if n := h.runsOf("f1"); n != 2 {
				t.Fatalf("f1 re-analyzed again with nothing changed (%d runs)", n)
			}
		})
	}
}

// baselineErrBackend fails every baseline read.
type baselineErrBackend struct{ *healthBackend }

func (baselineErrBackend) GetFlowBaseline(context.Context, string) (*storageif.FlowBaseline, error) {
	return nil, errors.New("baseline store down")
}

func (baselineErrBackend) GetFlowBaselinesBatch(context.Context, []string) (map[string]*storageif.FlowBaseline, error) {
	return nil, errors.New("baseline store down")
}

// TestScanOnce_RescansWhatItCouldNotCompletelyScan: a failed analysis records
// nothing, so the next sweep tries again; and a flow whose inputs could not
// all be read is never skipped, since its key is unknown.
func TestScanOnce_RescansWhatItCouldNotCompletelyScan(t *testing.T) {
	ctx := context.Background()

	b := newBackend()
	seedFlow(t, b, "f1")
	h := newSkipHarness(t, b)
	h.fail["f1"] = true
	h.s.ScanOnce(ctx)
	h.mu.Lock()
	h.fail["f1"] = false
	h.mu.Unlock()
	h.s.ScanOnce(ctx)
	h.s.ScanOnce(ctx)
	if n := h.runsOf("f1"); n != 2 {
		t.Fatalf("f1 analyzed %d times (fail, succeed, unchanged), want 2", n)
	}

	eb := baselineErrBackend{newBackend()}
	seedFlow(t, eb, "f1")
	analyzed := 0
	s := New(eb, nil, func(_ context.Context, doc *models.FlowDocument) (*models.AnalysisReport, error) {
		analyzed++
		return &models.AnalysisReport{FlowID: doc.ID}, nil
	}, nil, 0, loopbackChannels(eb))
	s.SetRuleSetDigest(func(context.Context, string) string { return "rules" })
	s.ScanOnce(ctx)
	s.ScanOnce(ctx)
	if analyzed != 2 {
		t.Fatalf("flow with an unreadable baseline analyzed %d times in two sweeps, want 2", analyzed)
	}
}

// TestScanOnce_WithoutARuleSetDigestNothingIsSkipped: a scanner that cannot
// see rule changes keeps re-analyzing every flow on every sweep.
func TestScanOnce_WithoutARuleSetDigestNothingIsSkipped(t *testing.T) {
	b := newBackend()
	seedFlow(t, b, "f1")
	var n int
	s := New(b, nil, func(_ context.Context, doc *models.FlowDocument) (*models.AnalysisReport, error) {
		n++
		return &models.AnalysisReport{FlowID: doc.ID}, nil
	}, nil, 0, loopbackChannels(b))
	s.ScanOnce(context.Background())
	s.ScanOnce(context.Background())
	if n != 2 {
		t.Fatalf("analyzed %d times in two sweeps, want 2", n)
	}
}

// TestScanOnce_PrunesScanKeysForDeletedFlows: like lastSig, the per-flow scan
// keys are dropped once a complete sweep no longer sees the flow.
func TestScanOnce_PrunesScanKeysForDeletedFlows(t *testing.T) {
	b := newBackend()
	seedFlow(t, b, "f1")
	h := newSkipHarness(t, b)
	h.s.ScanOnce(context.Background())
	if h.s.lastScanKey("f1") == "" {
		t.Fatal("no scan key recorded for f1")
	}
	if err := b.DeleteFlow(context.Background(), "f1"); err != nil {
		t.Fatal(err)
	}
	h.s.ScanOnce(context.Background())
	h.s.mu.Lock()
	defer h.s.mu.Unlock()
	if len(h.s.lastScan) != 0 {
		t.Fatalf("lastScan = %v, want empty after the deleted flow is pruned", h.s.lastScan)
	}
}

func mustCustomRule(t *testing.T, id, nameMatch string) analyzer.Rule {
	t.Helper()
	r, err := analyzer.NewCustomRule(analyzer.CustomRuleConfig{ID: id, Name: id, NameMatch: nameMatch})
	if err != nil {
		t.Fatalf("custom rule: %v", err)
	}
	return r
}

// TestDigestRuleSet covers what a rule-set change is: the set of rule IDs
// (in any order), a configured rule's configuration under an unchanged ID,
// and the per-rule settings.
func TestDigestRuleSet(t *testing.T) {
	builtins := analyzer.AllRules()
	if len(builtins) < 2 {
		t.Fatal("need at least two built-in rules")
	}
	settings := func(enabled bool) *models.AppSettings {
		return &models.AppSettings{Analysis: models.AnalysisSettings{Rules: map[string]models.RuleConfig{
			builtins[0].ID(): {Enabled: enabled, Severity: "warning", Options: map[string]interface{}{"max": 3}},
		}}}
	}
	base := DigestRuleSet(settings(true), builtins)

	reversed := append([]analyzer.Rule(nil), builtins...)
	for i, j := 0, len(reversed)-1; i < j; i, j = i+1, j-1 {
		reversed[i], reversed[j] = reversed[j], reversed[i]
	}
	if DigestRuleSet(settings(true), reversed) != base {
		t.Error("rule order changed the digest")
	}
	if DigestRuleSet(settings(false), builtins) == base {
		t.Error("disabling a rule did not change the digest")
	}
	if DigestRuleSet(settings(true), builtins[1:]) == base {
		t.Error("removing a rule did not change the digest")
	}
	if DigestRuleSet(nil, builtins) == base {
		t.Error("dropping the settings did not change the digest")
	}
	withFoo := DigestRuleSet(settings(true), append(append([]analyzer.Rule(nil), builtins...), mustCustomRule(t, "org-1", "Foo")))
	withBar := DigestRuleSet(settings(true), append(append([]analyzer.Rule(nil), builtins...), mustCustomRule(t, "org-1", "Bar")))
	if withFoo == base || withFoo == withBar {
		t.Error("a custom rule's configuration did not reach the digest")
	}
}

// panickySSE panics on EmitTo while armed.
type panickySSE struct{ armed bool }

func (p *panickySSE) EmitTo(string, string, any) {
	if p.armed {
		panic("sse push failed")
	}
}

// TestScanOnce_AnInterruptedScanIsNotRecorded: a scan that dies part-way
// (here a panic after the dedup state moved on, before the dispatch) must not
// leave the PREVIOUS scan's key in place — otherwise the restored state is
// skipped although lastSig holds the interrupted scan's signature. The
// restored drift is the first scan's alert, already in the inbox and
// delivered, so it is not delivered again.
func TestScanOnce_AnInterruptedScanIsNotRecorded(t *testing.T) {
	ctx := context.Background()
	b := newBackend()
	if err := b.SaveFlow(ctx, &storageif.FlowDocument{ID: "f1", Name: "f1", OwnerID: "u1",
		Content: json.RawMessage(`{"id":"f1","subflows":[]}`)}); err != nil {
		t.Fatal(err)
	}
	h := newSkipHarness(t, b)
	h.reports["f1"] = &models.AnalysisReport{FlowID: "f1", Findings: []models.Finding{
		{RuleID: "r1", BlockID: "b1", Severity: models.SeverityWarning},
		{RuleID: "r2", BlockID: "b2", Severity: models.SeverityError},
	}}
	sse := &panickySSE{}
	h.s.SetEventNotifier(sse)
	setBaseline := func(keys ...string) {
		if err := b.SetFlowBaseline(ctx, &storageif.FlowBaseline{FlowID: "f1", Keys: keys}); err != nil {
			t.Fatal(err)
		}
	}

	setBaseline("r1:b1") // one new error
	h.s.ScanOnce(ctx)
	if got := h.cap.all(); len(got) != 1 {
		t.Fatalf("first scan: %d alerts, want 1", len(got))
	}

	setBaseline() // one new error and one new warning; the push panics
	sse.armed = true
	h.s.ScanOnce(ctx)
	sse.armed = false

	setBaseline("r1:b1") // back to the first scan's inputs
	h.s.ScanOnce(ctx)
	if n := h.runsOf("f1"); n != 3 {
		t.Fatalf("f1 analyzed %d times, want 3", n)
	}
	if got := h.cap.all(); len(got) != 1 {
		t.Fatalf("restored drift delivered %d alerts in total, want 1 (the first scan's, already in the inbox)", len(got))
	}
	if h.s.lastScanKey("f1") == "" {
		t.Fatal("the restored scan was not recorded")
	}
}
