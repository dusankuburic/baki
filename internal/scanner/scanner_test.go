package scanner

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

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
