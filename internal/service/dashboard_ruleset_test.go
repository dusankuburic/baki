package service

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// TestRecordAnalysis_RecordsTheRuleSet: the health snapshot keeps the rule set it was scored with, so the scanner
// compares a later score like for like (ADR 0016).
func TestRecordAnalysis_RecordsTheRuleSet(t *testing.T) {
	backend := &captureDashBackend{FakeBackend: testutil.NewFakeBackend()}
	svc := NewDashboardService(backend, nil, nil, nil)
	rs := &models.RuleSet{Rules: []models.RuleRef{{ID: "wait-zero"}}, Digest: "rs1:d"}
	svc.RecordAnalysis(context.Background(), &models.FlowDocument{ID: "flow-1"},
		&models.AnalysisReport{FlowID: "flow-1", GeneratedAt: time.Now(), RuleSet: rs})
	if backend.saved == nil || backend.saved.RuleSet != rs {
		t.Fatalf("saved = %+v; want the report's rule set", backend.saved)
	}
}
