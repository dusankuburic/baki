package analyzer

import (
	"strings"
	"testing"

	"pad-core/models"
)

func TestGenerateHTMLReport(t *testing.T) {
	t.Run("empty_report", func(t *testing.T) {
		report := &models.AnalysisReport{
			FlowID: "test-flow",
			Stats:  models.AnalysisStats{},
		}
		html := GenerateHTMLReport(report)
		if !strings.Contains(html, "<!DOCTYPE html>") {
			t.Error("expected DOCTYPE")
		}
		if !strings.Contains(html, "test-flow") {
			t.Error("expected flow ID in output")
		}
		if !strings.Contains(html, "No findings") {
			t.Error("expected no-findings message")
		}
	})

	t.Run("with_findings", func(t *testing.T) {
		report := &models.AnalysisReport{
			FlowID: "f1",
			Stats: models.AnalysisStats{
				Errors:         1,
				Warnings:       2,
				Info:           1,
				BlocksAnalyzed: 10,
				RulesRun:       5,
			},
			Findings: []models.Finding{
				{RuleID: "R1", Severity: models.SeverityError, Title: "Test error", BlockID: "b1", Category: "Reliability"},
				{RuleID: "R2", Severity: models.SeverityWarning, Title: "Test warning", BlockID: "b2", Category: "Style"},
			},
			Metrics: &models.FlowMetrics{HealthScore: 85},
		}
		html := GenerateHTMLReport(report)
		if !strings.Contains(html, "severity-error") {
			t.Error("expected error severity class")
		}
		if !strings.Contains(html, "severity-warning") {
			t.Error("expected warning severity class")
		}
		if !strings.Contains(html, "Health Score: 85") {
			t.Error("expected health score")
		}
		if !strings.Contains(html, "Test error") {
			t.Error("expected finding title")
		}
	})

	t.Run("low_health_uses_bad_class", func(t *testing.T) {
		report := &models.AnalysisReport{
			FlowID:  "f2",
			Stats:   models.AnalysisStats{Errors: 5},
			Metrics: &models.FlowMetrics{HealthScore: 30},
		}
		html := GenerateHTMLReport(report)
		if !strings.Contains(html, "health-bad") {
			t.Error("expected health-bad class for score 30")
		}
	})
}

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
