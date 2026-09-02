package scanner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"pad-core/models"

	"pad-analyzer/internal/notify"
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
	d, err := notify.New(notify.Config{WebhookURL: url})
	if err != nil {
		t.Fatalf("notify.New: %v", err)
	}
	return d
}

func (c *capture) all() []notify.Event {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]notify.Event(nil), c.events...)
}

// healthBackend embeds the in-memory FakeBackend and adds prior-health lookups so
// the regression path can be exercised (FakeBackend's LoadFlowHealth is a no-op).
type healthBackend struct {
	*testutil.FakeBackend
	health map[string]*storageif.HealthSnapshot
}

func (b *healthBackend) LoadFlowHealth(_ context.Context, flowID string) (*storageif.HealthSnapshot, error) {
	return b.health[flowID], nil
}

func newBackend() *healthBackend {
	return &healthBackend{FakeBackend: testutil.NewFakeBackend(), health: map[string]*storageif.HealthSnapshot{}}
}

func seedFlow(t *testing.T, b storageif.StorageBackend, id string) {
	t.Helper()
	if err := b.SaveFlow(context.Background(), &storageif.FlowDocument{
		ID:      id,
		Name:    id,
		Content: json.RawMessage(`{"id":"` + id + `","subflows":[]}`),
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
	s := New(b, analyzeReturning(reports), mustNotifier(t, cap.srv.URL), 0)
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
	s := New(b, analyzeReturning(reports), mustNotifier(t, cap.srv.URL), 0)
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
	s := New(b, analyzeReturning(reports), mustNotifier(t, cap.srv.URL), 0)
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
	s := New(b, analyzeReturning(reports), mustNotifier(t, cap.srv.URL), 0)
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
	s := New(b, analyzeReturning(reports), mustNotifier(t, cap.srv.URL), 0)

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
	s := New(b, analyzeReturning(reports), mustNotifier(t, cap.srv.URL), 0)

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
	s := New(b, analyzeReturning(reports), mustNotifier(t, cap.srv.URL), 0)
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
	s := New(b, analyzeReturning(reports), mustNotifier(t, cap.srv.URL), 0)
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
	// Seed the org flow with OrganizationID set AT SAVE TIME (seedFlow copies
	// the doc; mutating the map entry post-hoc works too, but keeping the
	// fixture explicit documents the org-scoped shape).
	seedFlow(t, b, "f-org")
	if doc, err := b.LoadFlow(context.Background(), "f-org"); err == nil {
		doc.OrganizationID = "org-1"
		_ = b.SaveFlow(context.Background(), doc)
	}
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

	s := New(b, analyzeReturning(reports), mustNotifier(t, global.srv.URL), 0)
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
