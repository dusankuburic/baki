package analyzer

import (
	"strings"
	"testing"

	"pad-core/models"
)

func TestGenerateBatchHTMLReport(t *testing.T) {
	t.Run("multi_flow_batch", func(t *testing.T) {
		batch := &models.BatchAnalysis{
			TotalFlows:     2,
			TotalErrors:    1,
			TotalWarnings:  3,
			AvgHealthScore: 72.5,
			DurationMs:     150,
			Results: []models.BatchResult{
				{FlowID: "f1", FlowName: "Flow A", Report: &models.AnalysisReport{Stats: models.AnalysisStats{Errors: 1, Warnings: 2}, Metrics: &models.FlowMetrics{HealthScore: 65}}},
				{FlowID: "f2", FlowName: "Flow B", Report: &models.AnalysisReport{Stats: models.AnalysisStats{Warnings: 1}, Metrics: &models.FlowMetrics{HealthScore: 80}}},
			},
		}
		html := GenerateBatchHTMLReport(batch)
		if !strings.Contains(html, "Batch Analysis") {
			t.Error("expected batch title")
		}
		if !strings.Contains(html, "Flow A") {
			t.Error("expected Flow A")
		}
		if !strings.Contains(html, "1 errors") {
			t.Error("expected error badge")
		}
	})
}
