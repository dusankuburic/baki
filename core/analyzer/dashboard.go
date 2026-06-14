package analyzer

import (
	"sort"

	"pad-core/models"
)

func ComputeDashboard(reports []*models.AnalysisReport) *models.DashboardStats {
	if len(reports) == 0 {
		return &models.DashboardStats{
			FindingsBySeverity: map[string]int{},
			FindingsByCategory: map[string]int{},
			FindingsByRule:     map[string]int{},
		}
	}

	stats := &models.DashboardStats{
		TotalFlowsAnalyzed: len(reports),
		FindingsBySeverity: make(map[string]int),
		FindingsByCategory: make(map[string]int),
		FindingsByRule:     make(map[string]int),
	}

	var healthSum float64
	healthCount := 0

	for _, r := range reports {
		stats.TotalFindings += len(r.Findings)
		for _, f := range r.Findings {
			stats.FindingsBySeverity[string(f.Severity)]++
			if f.Category != "" {
				stats.FindingsByCategory[f.Category]++
			}
			stats.FindingsByRule[f.RuleID]++
		}
		if r.Metrics != nil {
			healthSum += float64(r.Metrics.HealthScore)
			healthCount++
			stats.TotalSubflows += r.Metrics.SubflowCount
		}
	}

	if healthCount > 0 {
		stats.AvgHealthScore = healthSum / float64(healthCount)
	}

	var problems []models.ProblemFlow
	for _, r := range reports {
		if len(r.Findings) > 0 {
			hs := 0
			if r.Metrics != nil {
				hs = r.Metrics.HealthScore
			}
			problems = append(problems, models.ProblemFlow{
				FlowID:       r.FlowID,
				FlowName:     r.FlowName,
				FindingCount: len(r.Findings),
				HealthScore:  hs,
			})
		}
	}
	sort.Slice(problems, func(i, j int) bool {
		return problems[i].FindingCount > problems[j].FindingCount
	})
	if len(problems) > 10 {
		problems = problems[:10]
	}
	stats.TopProblemFlows = problems

	return stats
}
