package service

import (
	"context"
	"math"
	"sort"

	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/models"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// DashboardService assembles the welcome ("home") dashboard payload and persists
// per-flow analysis summaries that back it.
//
// Mode is inferred from the backend: in cloud mode the backend is a Postgres
// store and health/findings come from the durable flow_analysis table (so the
// dashboard is populated on first load across sessions and replicas). In
// local/desktop mode the backend is nil; health/findings come from the
// in-process analyzer cache via AnalysisService.ComputeDashboard, and token
// usage is not tracked.
type DashboardService struct {
	backend  storageif.StorageBackend
	analysis *AnalysisService
}

func NewDashboardService(backend storageif.StorageBackend, analysis *AnalysisService) *DashboardService {
	return &DashboardService{backend: backend, analysis: analysis}
}

const dashboardTokenDays = 14

// RecordAnalysis persists a flow's latest analysis summary so the dashboard can
// show it later. Best-effort: failures are logged, never surfaced to the caller,
// and a no-op in local mode (the session cache is the source there).
func (s *DashboardService) RecordAnalysis(ctx context.Context, doc *models.FlowDocument, report *models.AnalysisReport) {
	if s.backend == nil || doc == nil || report == nil {
		return
	}
	health := 0
	if report.Metrics != nil {
		health = report.Metrics.HealthScore
	}
	byCat := make(map[string]int)
	for _, f := range report.Findings {
		cat := f.Category
		if cat == "" {
			cat = "other"
		}
		byCat[cat]++
	}
	fa := &storageif.FlowAnalysis{
		FlowID:      doc.ID,
		HealthScore: health,
		Errors:      report.Stats.Errors,
		Warnings:    report.Stats.Warnings,
		Info:        report.Stats.Info,
		ByCategory:  byCat,
		AnalyzedAt:  report.GeneratedAt,
	}
	if err := s.backend.SaveFlowAnalysis(ctx, fa); err != nil {
		logger.Warn("dashboard: persist flow analysis failed", "flowId", doc.ID, "err", err)
	}
}

// BuildHome assembles the dashboard payload for userID. It never hard-fails: on
// a data-source error it logs and returns a valid, sparse payload (availability
// flags false) so the UI can render honest empty states rather than an error.
func (s *DashboardService) BuildHome(ctx context.Context, userID string) *models.DashboardHomeData {
	if s.backend == nil {
		return s.buildLocal()
	}
	return s.buildCloud(ctx, userID)
}

func (s *DashboardService) buildCloud(ctx context.Context, userID string) *models.DashboardHomeData {
	out := emptyHome()
	data, err := s.backend.FlowDashboardData(ctx, userID, dashboardTokenDays)
	if err != nil {
		logger.Warn("dashboard: build cloud data failed", "userId", userID, "err", err)
		return out
	}
	out.Overview = models.DashboardOverview{
		AvgHealthScore:  data.AvgHealth,
		HealthAvailable: data.HealthCount > 0,
		TotalFlows:      data.TotalFlows,
		TotalSubflows:   data.TotalSubflows,
	}
	out.Findings = models.DashboardFindingsAgg{
		Available:  data.HealthCount > 0,
		BySeverity: map[string]int{"error": data.Errors, "warning": data.Warnings, "info": data.Info},
		ByCategory: sortedCategories(data.ByCategory),
	}
	for _, r := range data.Recent {
		out.RecentFlows = append(out.RecentFlows, models.RecentFlowStub{
			ID:          r.FlowID,
			Name:        r.Name,
			HealthScore: r.HealthScore,
			UpdatedAt:   r.UpdatedAt,
		})
	}
	for _, t := range data.TokenUsage {
		out.TokenUsage = append(out.TokenUsage, models.DailyTokenUsage{
			Date:      t.Date,
			TokensIn:  t.TokensIn,
			TokensOut: t.TokensOut,
		})
	}
	return out
}

func (s *DashboardService) buildLocal() *models.DashboardHomeData {
	out := emptyHome()
	if s.analysis == nil {
		return out
	}
	stats := s.analysis.ComputeDashboard()
	out.Overview = models.DashboardOverview{
		AvgHealthScore:  int(math.Round(stats.AvgHealthScore)),
		HealthAvailable: stats.TotalFlowsAnalyzed > 0,
		TotalFlows:      stats.TotalFlowsAnalyzed,
		TotalSubflows:   stats.TotalSubflows,
	}
	out.Findings = models.DashboardFindingsAgg{
		Available:  stats.TotalFindings > 0,
		BySeverity: copyIntMap(stats.FindingsBySeverity),
		ByCategory: sortedCategories(stats.FindingsByCategory),
	}
	return out
}

// emptyHome returns a payload with non-nil maps/slices so the JSON carries
// `[]`/`{}` rather than `null`, keeping the frontend's length checks simple.
func emptyHome() *models.DashboardHomeData {
	return &models.DashboardHomeData{
		TokenUsage:  []models.DailyTokenUsage{},
		RecentFlows: []models.RecentFlowStub{},
		Findings: models.DashboardFindingsAgg{
			BySeverity: map[string]int{},
			ByCategory: []models.FindingCategory{},
		},
	}
}

// sortedCategories renders a category→count map as a stable slice ordered by
// count descending then name, so the radar chart's axes don't reshuffle between
// requests.
func sortedCategories(m map[string]int) []models.FindingCategory {
	out := make([]models.FindingCategory, 0, len(m))
	for cat, n := range m {
		out = append(out, models.FindingCategory{Category: cat, Count: n})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Category < out[j].Category
	})
	return out
}

func copyIntMap(m map[string]int) map[string]int {
	out := make(map[string]int, len(m))
	for k, v := range m {
		out[k] = v
	}
	return out
}
