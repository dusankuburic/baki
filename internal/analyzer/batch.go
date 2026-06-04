package analyzer

import (
	"time"

	"pad-analyzer/internal/models"
)

func RunBatchAnalysis(docs []*models.FlowDocument, rules []Rule, settings *models.AppSettings) *models.BatchAnalysis {
	start := time.Now()

	results := make([]models.BatchResult, 0, len(docs))
	var totalFindings, totalErrors, totalWarnings, totalInfo int
	var healthSum float64
	healthCount := 0

	for _, doc := range docs {
		report := RunAnalysis(doc, rules, settings, nil)
		result := models.BatchResult{
			FlowID:   doc.ID,
			FlowName: doc.Name,
			Report:   report,
		}
		results = append(results, result)

		totalFindings += len(report.Findings)
		totalErrors += report.Stats.Errors
		totalWarnings += report.Stats.Warnings
		totalInfo += report.Stats.Info
		if report.Metrics != nil {
			healthSum += float64(report.Metrics.HealthScore)
			healthCount++
		}
	}

	avgHealth := 0.0
	if healthCount > 0 {
		avgHealth = healthSum / float64(healthCount)
	}

	return &models.BatchAnalysis{
		Results:        results,
		TotalFlows:     len(docs),
		TotalFindings:  totalFindings,
		TotalErrors:    totalErrors,
		TotalWarnings:  totalWarnings,
		TotalInfo:      totalInfo,
		AvgHealthScore: avgHealth,
		DurationMs:     int(time.Since(start).Milliseconds()),
	}
}

func DiffReports(old, new *models.AnalysisReport) *models.AnalysisDiff {
	diff := &models.AnalysisDiff{
		FlowID: new.FlowID,
	}

	oldMap := make(map[string]models.Finding)
	for _, f := range old.Findings {
		key := f.RuleID + ":" + f.BlockID
		oldMap[key] = f
	}

	newMap := make(map[string]models.Finding)
	for _, f := range new.Findings {
		key := f.RuleID + ":" + f.BlockID
		newMap[key] = f
	}

	for key, f := range newMap {
		if _, exists := oldMap[key]; exists {
			diff.Persisted = append(diff.Persisted, f)
		} else {
			diff.Added = append(diff.Added, f)
		}
	}

	for key, f := range oldMap {
		if _, exists := newMap[key]; !exists {
			diff.Removed = append(diff.Removed, f)
		}
	}

	diff.AddedCount = len(diff.Added)
	diff.RemovedCount = len(diff.Removed)
	diff.PersistedCount = len(diff.Persisted)

	if diff.Added == nil {
		diff.Added = []models.Finding{}
	}
	if diff.Removed == nil {
		diff.Removed = []models.Finding{}
	}
	if diff.Persisted == nil {
		diff.Persisted = []models.Finding{}
	}
	return diff
}
