package service

import (
	"context"
	"math"
	"sort"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
	"pad-core/models"
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
	flowSvc  *FlowService
}

func NewDashboardService(backend storageif.StorageBackend, analysis *AnalysisService, flowSvc *FlowService) *DashboardService {
	return &DashboardService{backend: backend, analysis: analysis, flowSvc: flowSvc}
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
	byRule := make(map[string]int)
	for _, f := range report.Findings {
		cat := f.Category
		if cat == "" {
			cat = "other"
		}
		byCat[cat]++
		if f.RuleID != "" {
			byRule[f.RuleID]++
		}
	}
	fa := &storageif.FlowAnalysis{
		FlowID:      doc.ID,
		HealthScore: health,
		Errors:      report.Stats.Errors,
		Warnings:    report.Stats.Warnings,
		Info:        report.Stats.Info,
		ByCategory:  byCat,
		ByRule:      byRule,
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
		out := s.buildLocal()
		out.IsCloud = false
		return out
	}
	out := s.buildCloud(ctx, userID)
	out.IsCloud = true
	return out
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

	// Advanced sections (best-effort; a failure logs and leaves empty slices).
	adv, err := s.backend.FlowDashboardAdvanced(ctx, userID, 30)
	if err != nil {
		logger.Warn("dashboard: advanced data failed", "userId", userID, "err", err)
		return out
	}
	for _, h := range adv.HealthTrend {
		out.HealthTrend = append(out.HealthTrend, models.DailyHealthPoint{
			Date: h.Date, AvgHealth: h.AvgHealth, FlowCount: h.FlowCount,
		})
	}
	for _, c := range adv.CostByProv {
		out.CostByProv = append(out.CostByProv, models.ProviderCostStub{
			Provider: c.Provider, Cost: c.Cost, TokensIn: c.TokensIn, TokensOut: c.TokensOut,
		})
	}
	for _, r := range adv.RuleFreq {
		out.RuleFreq = append(out.RuleFreq, models.RuleFrequencyStub{Rule: r.Rule, Count: r.Count})
	}
	for _, a := range adv.Activity {
		out.Activity = append(out.Activity, models.ActivityStub{
			Action: a.Action, FlowName: a.FlowName, CreatedAt: a.CreatedAt,
		})
	}
	for _, c := range adv.Complexity {
		out.Complexity = append(out.Complexity, models.FlowComplexityStub{
			FlowID: c.FlowID, FlowName: c.FlowName, BlockCount: c.BlockCount,
			FindingCount: c.FindingCount, HealthScore: c.HealthScore,
		})
	}
	out.Security = models.DashboardSecurityStub{
		FailedLogins24h:    adv.Security.FailedLogins24h,
		CredentialFindings: adv.Security.CredentialFindings,
	}

	return out
}

func (s *DashboardService) buildLocal() *models.DashboardHomeData {
	out := emptyHome()
	if s.analysis == nil {
		return out
	}
	// Single cache snapshot — ensures all dashboard sections see the same data
	// and deduplicates flows re-analyzed after edits or settings changes.
	stats, reports := s.analysis.DashboardData()
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

	// Rule frequency — derived from the same in-memory stats that feed the
	// Analytics Dashboard. Sorted by count descending so the top offenders
	// appear first.
	out.RuleFreq = sortedRuleFreq(stats.FindingsByRule)

	// Per-flow complexity scatter — derived from the SAME deduped snapshot
	// that fed the stats above, so the two sections never disagree.
	for _, r := range reports {
		blockCount := 0
		healthScore := 0
		if r.Metrics != nil {
			blockCount = r.Metrics.TotalBlocks
			healthScore = r.Metrics.HealthScore
		}
		out.Complexity = append(out.Complexity, models.FlowComplexityStub{
			FlowID:       r.FlowID,
			FlowName:     r.FlowName,
			BlockCount:   blockCount,
			FindingCount: len(r.Findings),
			HealthScore:  healthScore,
		})
	}

	// Recent flows — from the settings store (same source as the sidebar
	// "Recent" list). Only populated when the flow service is available
	// (always true in local mode via DI).
	if s.flowSvc != nil {
		files, err := s.flowSvc.RecentFiles()
		if err == nil {
			for _, f := range files {
				// No health score in local mode (the flow may not have been
				// analyzed yet); leave nil so the UI shows a dash.
				out.RecentFlows = append(out.RecentFlows, models.RecentFlowStub{
					ID:        f.Path,
					Name:      f.Name,
					UpdatedAt: f.LastOpen,
				})
			}
		}
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
		HealthTrend: []models.DailyHealthPoint{},
		CostByProv:  []models.ProviderCostStub{},
		RuleFreq:    []models.RuleFrequencyStub{},
		Activity:    []models.ActivityStub{},
		Complexity:  []models.FlowComplexityStub{},
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

// sortedRuleFreq renders a rule→count map as a stable slice ordered by count
// descending then rule name, capping at 15 entries so the card doesn't overflow.
func sortedRuleFreq(m map[string]int) []models.RuleFrequencyStub {
	out := make([]models.RuleFrequencyStub, 0, len(m))
	for rule, count := range m {
		out = append(out, models.RuleFrequencyStub{Rule: rule, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Rule < out[j].Rule
	})
	if len(out) > 15 {
		out = out[:15]
	}
	return out
}
