package service

import (
	"context"
	"testing"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// captureDashBackend records the last FlowAnalysis handed to SaveFlowAnalysis
// so RecordAnalysis's enrichment (confidence/fixability/total) can be asserted
// without a real Postgres round-trip.
type captureDashBackend struct {
	*testutil.FakeBackend
	saved *storageif.FlowAnalysis
}

func (c *captureDashBackend) SaveFlowAnalysis(_ context.Context, fa *storageif.FlowAnalysis) error {
	c.saved = fa
	return nil
}

// TestRecordAnalysis_PersistsConfidenceAndFixability verifies the new v4
// rollup fields are populated from a report's findings: ByConfidence tallies
// each finding's tier, AutoFixableCount counts findings with a one-click fix,
// and TotalFindings equals errors+warnings+info.
func TestRecordAnalysis_PersistsConfidenceAndFixability(t *testing.T) {
	backend := &captureDashBackend{FakeBackend: testutil.NewFakeBackend()}
	svc := NewDashboardService(backend, nil, nil)

	doc := &models.FlowDocument{ID: "flow-1"}
	report := &models.AnalysisReport{
		FlowID:      "flow-1",
		GeneratedAt: time.Now(),
		Findings: []models.Finding{
			{RuleID: "missing-timeout", Severity: models.SeverityWarning, Category: "Reliability", Confidence: models.ConfidenceMedium, AutoFix: "set-timeout"},
			{RuleID: "resource-leak", Severity: models.SeverityWarning, Category: "Reliability", Confidence: models.ConfidenceHigh, AutoFix: "insert-close"},
			{RuleID: "hardcoded-url", Severity: models.SeverityInfo, Category: "Portability", Confidence: models.ConfidenceLow},
			{RuleID: "deep-nesting", Severity: models.SeverityInfo, Category: "Style"}, // empty Confidence → treated as medium
		},
	}
	report.Stats.Errors = 0
	report.Stats.Warnings = 2
	report.Stats.Info = 2

	svc.RecordAnalysis(context.Background(), doc, report)

	if backend.saved == nil {
		t.Fatal("SaveFlowAnalysis was not called")
	}
	got := backend.saved

	if got.TotalFindings != 4 {
		t.Errorf("TotalFindings = %d, want 4", got.TotalFindings)
	}
	if got.AutoFixableCount != 2 {
		t.Errorf("AutoFixableCount = %d, want 2", got.AutoFixableCount)
	}
	wantConf := map[string]int{"high": 1, "medium": 2, "low": 1}
	if len(got.ByConfidence) != len(wantConf) {
		t.Errorf("ByConfidence = %v, want %v", got.ByConfidence, wantConf)
	}
	for k, want := range wantConf {
		if got.ByConfidence[k] != want {
			t.Errorf("ByConfidence[%q] = %d, want %d", k, got.ByConfidence[k], want)
		}
	}
	// The pre-existing category/rule maps must still be populated.
	if got.ByCategory["Reliability"] != 2 {
		t.Errorf("ByCategory[Reliability] = %d, want 2", got.ByCategory["Reliability"])
	}
}

// TestRecordAnalysis_NilSafeDocument ensures the best-effort contract holds:
// a nil document or report is a no-op, never a panic.
func TestRecordAnalysis_NilSafeDocument(t *testing.T) {
	backend := &captureDashBackend{FakeBackend: testutil.NewFakeBackend()}
	svc := NewDashboardService(backend, nil, nil)

	svc.RecordAnalysis(context.Background(), nil, &models.AnalysisReport{})
	svc.RecordAnalysis(context.Background(), &models.FlowDocument{ID: "x"}, nil)

	if backend.saved != nil {
		t.Error("expected no SaveFlowAnalysis call for nil doc/report")
	}
}
