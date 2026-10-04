package analyzer

import (
	"encoding/json"
	"fmt"
	"testing"

	"pad-core/models"
)

// dedupTestReport analyzes a flow whose findings include folded duplicates
// (one rule firing twice per block for the same subject).
func dedupTestReport(t *testing.T) *models.AnalysisReport {
	t.Helper()
	report := RunAnalysis(buildLargeFlow(300), append(AllRules(), dupRule{}), nil, nil)
	if len(report.Findings) == 0 {
		t.Fatal("expected findings")
	}
	return report
}

// dupRule emits the same finding twice per block, so dedup folds one copy.
type dupRule struct{}

func (dupRule) ID() string                       { return "test-dup" }
func (dupRule) Name() string                     { return "Test dup" }
func (dupRule) Description() string              { return "" }
func (dupRule) DefaultSeverity() models.Severity { return models.SeverityInfo }
func (dupRule) Category() string                 { return "test" }
func (dupRule) Check(b *models.Block, _ *RuleContext) []models.Finding {
	f := models.Finding{RuleID: "test-dup", Title: "dup", Severity: models.SeverityInfo, BlockID: b.ID, SubflowID: b.SubflowID}
	return []models.Finding{f, f}
}

// TestAnalysisReport_NoGroupsSerialized: the per-block groups used to be
// materialized on every report (58% of its JSON when they were serialized,
// 44% of its heap once they were not) and no client or in-process reader
// used them; the dedup view calls /api/analysis/deduplicate. The report must
// neither carry nor serialize them.
func TestAnalysisReport_NoGroupsSerialized(t *testing.T) {
	report := dedupTestReport(t)
	raw, err := json.Marshal(report)
	if err != nil {
		t.Fatal(err)
	}
	var top map[string]json.RawMessage
	if err := json.Unmarshal(raw, &top); err != nil {
		t.Fatal(err)
	}
	if _, ok := top["groups"]; ok {
		t.Errorf("report JSON carries \"groups\" (%d bytes of %d)", len(top["groups"]), len(raw))
	}
	if _, ok := top["findings"]; !ok {
		t.Error("report JSON lost \"findings\"")
	}
}

// TestReportFindings_StampedAndDeduplicated: the engine folds same-block,
// same-subject duplicates before stats, keeps a dense F-001.. numbering of
// the kept findings, and stamps every kept finding with its content-derived
// fields. Groups computed on demand from the report's findings carry the
// same stamped findings.
func TestReportFindings_StampedAndDeduplicated(t *testing.T) {
	report := dedupTestReport(t)
	dupFindings := 0
	for i, f := range report.Findings {
		if want := fmt.Sprintf("F-%03d", i+1); f.ID != want {
			t.Fatalf("finding %d has ID %q, want %q", i, f.ID, want)
		}
		if f.Fingerprint == "" || f.Confidence == "" {
			t.Fatalf("finding %s/%s is unstamped: fingerprint=%q confidence=%q", f.RuleID, f.Title, f.Fingerprint, f.Confidence)
		}
		if f.RuleID == "test-dup" {
			dupFindings++
		}
	}
	// dupRule fires twice per block; exactly one copy per block survives.
	if dupFindings != report.Stats.BlocksAnalyzed {
		t.Fatalf("test-dup findings = %d, want one per analyzed block (%d)", dupFindings, report.Stats.BlocksAnalyzed)
	}
	if total := report.Stats.Errors + report.Stats.Warnings + report.Stats.Info; total != len(report.Findings) {
		t.Fatalf("stats count %d findings, report has %d", total, len(report.Findings))
	}

	deduped, groups := DeduplicateFindings(report.Findings)
	if len(deduped) != len(report.Findings) {
		t.Fatalf("re-deduplicating the report's findings dropped %d of them", len(report.Findings)-len(deduped))
	}
	byID := make(map[string]models.Finding, len(report.Findings))
	for _, f := range report.Findings {
		byID[f.ID] = f
	}
	grouped := 0
	for _, g := range groups {
		if g.Primary == nil || g.Primary.ID == "" {
			t.Fatalf("group %s primary is unstamped: %+v", g.BlockID, g.Primary)
		}
		for _, f := range g.Findings {
			grouped++
			kept, ok := byID[f.ID]
			if !ok || kept.Fingerprint != f.Fingerprint || kept.RuleID != f.RuleID {
				t.Fatalf("group finding %s does not match the report's finding: %+v vs %+v", f.ID, f, kept)
			}
		}
	}
	if grouped != len(report.Findings) {
		t.Fatalf("on-demand groups cover %d findings, want %d", grouped, len(report.Findings))
	}
}
