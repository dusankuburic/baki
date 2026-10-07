package service

import (
	"context"
	"golang.org/x/sync/errgroup"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/singleflight"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/logger"
	"pad-core/models"
)

// dashboardCacheTTL bounds how long a stale dashboard result is served — the
// landing page triggers ~16 sequential DB queries, so 30s collapses re-loads.
const dashboardCacheTTL = 30 * time.Second

type dashCacheEntry struct {
	data      *models.DashboardHomeData
	fetchedAt time.Time
}

// DashboardService assembles the welcome ("home") dashboard payload and persists per-flow analysis
// summaries — cloud reads the durable flow_analysis table, local (nil backend) the analyzer cache.
type DashboardService struct {
	backend  storageif.DashboardStore
	analysis *AnalysisService
	flowSvc  *FlowService

	dashMu    sync.Mutex
	dashCache map[string]*dashCacheEntry
	// anaCache holds analytics payloads keyed by user + canonical query
	// (dashboard_analytics.go); shares dashMu and the TTL, empties with the home entry.
	anaCache map[string]*anaCacheEntry
	// anaSeq numbers anaCache inserts so entry-cap eviction has a deterministic
	// tie-break when entries share one fetchedAt; map order otherwise picked the victim.
	anaSeq uint64
	anaSF  singleflight.Group
	scope  ScopeResolver
	// dashSF collapses concurrent cold-cache builds per user — one buildCloud
	// run on a miss, not a thundering herd of ~16 DB queries per caller.
	dashSF singleflight.Group
}

// NewDashboardService builds the dashboard service; scope collects the analytics page's flow set (DI passes the
// library's FlowScopeResolver; without one BuildAnalytics fails).
func NewDashboardService(backend storageif.DashboardStore, analysis *AnalysisService, flowSvc *FlowService, scope ScopeResolver) *DashboardService {
	return &DashboardService{
		backend:   backend,
		analysis:  analysis,
		flowSvc:   flowSvc,
		scope:     scope,
		dashCache: make(map[string]*dashCacheEntry),
		anaCache:  make(map[string]*anaCacheEntry),
	}
}

const dashboardTokenDays = 14

// fixabilityStub combines finding-side counts with rule-side counts from the
// static catalog so "11 of 29 rules" is exact; shared by both modes, nil-safe.
func (s *DashboardService) fixabilityStub(available, total int) models.FixabilityStub {
	totalRules, autoFixableRules := 0, 0
	if s.analysis != nil {
		rs := s.analysis.GetRulesSummary()
		totalRules, autoFixableRules = rs.TotalRules, rs.AutoFixableRules
	}
	return models.FixabilityStub{
		Available:        available,
		Total:            total,
		AutoFixableRules: autoFixableRules,
		TotalRules:       totalRules,
	}
}

// ruleSeverities maps rule ID → effective severity from the live catalog (with
// user overrides, so the tint matches the findings). Nil when no analysis service.
func (s *DashboardService) ruleSeverities() map[string]string {
	if s.analysis == nil {
		return nil
	}
	rules := s.analysis.GetRules()
	out := make(map[string]string, len(rules))
	for _, r := range rules {
		out[r.ID] = string(r.DefaultSeverity)
	}
	return out
}

// RecordAnalysis persists a flow's latest analysis summary for the dashboard —
// best-effort, a no-op in local mode (session cache is the source there).
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
	byConf := make(map[string]int)
	autoFixable := 0
	for _, f := range report.Findings {
		cat := f.Category
		if cat == "" {
			cat = "other"
		}
		byCat[cat]++
		if f.RuleID != "" {
			byRule[f.RuleID]++
		}
		// Confidence drives the "how much to trust" donut; a finding with no
		// explicit confidence counts as the engine default (medium).
		conf := string(f.Confidence)
		if conf == "" {
			conf = string(models.ConfidenceMedium)
		}
		byConf[conf]++
		if f.AutoFix != "" {
			autoFixable++
		}
	}
	fa := &storageif.FlowAnalysis{
		FlowID:           doc.ID,
		HealthScore:      health,
		Errors:           report.Stats.Errors,
		Warnings:         report.Stats.Warnings,
		Info:             report.Stats.Info,
		ByCategory:       byCat,
		ByRule:           byRule,
		ByConfidence:     byConf,
		AutoFixableCount: autoFixable,
		TotalFindings:    report.Stats.Errors + report.Stats.Warnings + report.Stats.Info,
		AnalyzedAt:       report.GeneratedAt,
	}
	if err := s.backend.SaveFlowAnalysis(ctx, fa); err != nil {
		logger.Warn("dashboard: persist flow analysis failed", "flowId", doc.ID, "err", err)
	}
	// This owner's summaries just changed — drop their cached payload so the
	// next load reflects it instead of waiting out the TTL.
	if doc.OwnerID != "" {
		s.InvalidateDashboardCache(doc.OwnerID)
	}
}

// BuildHome assembles the dashboard payload for userID; it never hard-fails —
// on error it returns a valid sparse payload so the UI renders honest empty states.
func (s *DashboardService) BuildHome(ctx context.Context, userID string) *models.DashboardHomeData {
	if s.backend == nil {
		out := s.buildLocal()
		out.IsCloud = false
		return out
	}

	// Check TTL cache before hitting the database (16 queries → 0 on hit).
	s.dashMu.Lock()
	if entry, ok := s.dashCache[userID]; ok && time.Since(entry.fetchedAt) < dashboardCacheTTL {
		s.dashMu.Unlock()
		// Do NOT mutate entry.data here: it is a shared pointer handed to every
		// concurrent caller, and writing after unlocking races their JSON marshal.
		return entry.data
	}
	s.dashMu.Unlock()

	// Single-flight the cold build: concurrent cold-cache requests for the same
	// user share one buildCloud run instead of each issuing ~16 DB queries.
	res, _, _ := s.dashSF.Do("home:"+userID, func() (any, error) {
		out := s.buildCloud(ctx, userID)
		out.IsCloud = true
		// Store in cache (even on partial failure — the emptyHome stub is cheap
		// to serve for 30s and avoids hammering the DB on a transient error).
		s.dashMu.Lock()
		s.dashCache[userID] = &dashCacheEntry{data: out, fetchedAt: time.Now()}
		// The TTL governs freshness, not lifetime — sweep entries past it at
		// write time or the map grows with every distinct user forever.
		for id, entry := range s.dashCache {
			if time.Since(entry.fetchedAt) >= dashboardCacheTTL {
				delete(s.dashCache, id)
			}
		}
		s.dashMu.Unlock()
		return out, nil
	})
	if res != nil {
		return res.(*models.DashboardHomeData)
	}
	// Single-flight returned nil (panic/error path); fall back to a valid shape.
	return emptyHome()
}

// InvalidateDashboardCache clears the cached dashboard data for a user (all
// users when empty) — after settings changes or uploads that change the view.
func (s *DashboardService) InvalidateDashboardCache(userID string) {
	s.dashMu.Lock()
	defer s.dashMu.Unlock()
	if userID == "" {
		s.dashCache = make(map[string]*dashCacheEntry)
		s.anaCache = make(map[string]*anaCacheEntry)
	} else {
		delete(s.dashCache, userID)
		// Every analytics view this user built (any scope or filter) is keyed
		// under their id; other members' org-scoped views wait out the TTL.
		prefix := userID + "\x00"
		for k := range s.anaCache {
			if strings.HasPrefix(k, prefix) {
				delete(s.anaCache, k)
			}
		}
	}
}

func (s *DashboardService) buildCloud(ctx context.Context, userID string) *models.DashboardHomeData {
	out := emptyHome()
	// B1.5: the base and advanced queries are independent — run them
	// concurrently (16 sequential round-trips used to gate every cold load).
	var (
		data *storageif.DashboardData
		adv  *storageif.DashboardAdvancedData
	)
	var dataErr, advErr error
	g, gctx := errgroup.WithContext(ctx)
	g.Go(func() error {
		data, dataErr = s.backend.FlowDashboardData(gctx, userID, dashboardTokenDays)
		return dataErr
	})
	g.Go(func() error {
		adv, advErr = s.backend.FlowDashboardAdvanced(gctx, userID, 30)
		return advErr
	})
	_ = g.Wait()
	// Best-effort halves: the base sections render when the base fetch made
	// it, advanced appended when its fetch made it; each failure leaves its half empty.
	if dataErr != nil {
		logger.Warn("dashboard: build cloud data failed", "userId", userID, "err", dataErr)
	}
	if advErr != nil {
		logger.Warn("dashboard: advanced data failed", "userId", userID, "err", advErr)
	}
	if data == nil {
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
	// The base-only sections derived from `data` must still render when the
	// advanced half failed; everything below reads `adv`, which is nil then.
	out.ConfidenceDist = copyIntMap(data.Confidence)
	for _, hb := range data.HealthBuckets {
		out.HealthBuckets = append(out.HealthBuckets, models.HealthBucketStub{
			Label: hb.Label, Lo: hb.Lo, Hi: hb.Hi, Count: hb.Count,
		})
	}
	out.Fixability = s.fixabilityStub(data.AutoFixable, data.TotalFindings)
	if adv == nil {
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
	// Severity tint comes from the rule catalog, not the DB: the by_rule rollup
	// stores only counts, and severity is a per-rule property anyway.
	sev := s.ruleSeverities()
	for _, r := range adv.RuleFreq {
		out.RuleFreq = append(out.RuleFreq, models.RuleFrequencyStub{Rule: r.Rule, Count: r.Count, TopSeverity: sev[r.Rule]})
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
		// Available only when the backend actually read every count; a failed
		// count leaves a 0 that would read as "no issues".
		Available:          adv.Security.Available,
		FailedLogins24h:    adv.Security.FailedLogins24h,
		CredentialFindings: adv.Security.CredentialFindings,
		LockedAccounts:     adv.Security.LockedAccounts,
	}

	// v3 analytics sections.
	for _, p := range adv.SeverityTrend {
		out.SeverityTrend = append(out.SeverityTrend, models.DailySeverityPoint{
			Date: p.Date, Errors: p.Errors, Warnings: p.Warnings, Info: p.Info,
		})
	}

	// Workflow (cloud-only team-triage funnel + MTTR): Available once any
	// finding has been triaged; local mode never sets it (no persistent triage).
	wfTotal := 0
	for _, n := range adv.Workflow.Funnel {
		wfTotal += n
	}
	out.Workflow = models.WorkflowStub{
		Available:     wfTotal > 0,
		Funnel:        copyIntMap(adv.Workflow.Funnel),
		MttrHours:     adv.Workflow.MttrHours,
		ResolvedCount: adv.Workflow.ResolvedCount,
		StaleCount:    adv.Workflow.StaleCount,
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

	// Rule frequency — same in-memory stats that feed the Analytics Dashboard,
	// sorted by count descending so the top offenders appear first.
	out.RuleFreq = sortedRuleFreq(stats.FindingsByRule, s.ruleSeverities())

	// Per-flow complexity scatter — derived from the SAME deduped snapshot
	// that fed the stats above, so the two sections never disagree.
	healthScores := make([]int, 0, len(reports))
	for _, r := range reports {
		blockCount := 0
		healthScore := 0
		if r.Metrics != nil {
			blockCount = r.Metrics.TotalBlocks
			healthScore = r.Metrics.HealthScore
		}
		healthScores = append(healthScores, healthScore)
		out.Complexity = append(out.Complexity, models.FlowComplexityStub{
			FlowID:       r.FlowID,
			FlowName:     r.FlowName,
			BlockCount:   blockCount,
			FindingCount: len(r.Findings),
			HealthScore:  healthScore,
		})
	}

	// v3 analytics from the session cache: confidence distribution, fix
	// availability, health histogram. No per-day series locally, so trends stay empty.
	conf, totalFindings, autoFixable := analyticsFromReports(reports)
	out.ConfidenceDist = conf
	out.HealthBuckets = bucketHealth(healthScores)
	out.Fixability = s.fixabilityStub(autoFixable, totalFindings)

	// Recent flows — from the settings store (the sidebar "Recent" source);
	// only when the flow service is wired (always true in local mode via DI).
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
		HealthTrend:    []models.DailyHealthPoint{},
		CostByProv:     []models.ProviderCostStub{},
		RuleFreq:       []models.RuleFrequencyStub{},
		Activity:       []models.ActivityStub{},
		Complexity:     []models.FlowComplexityStub{},
		SeverityTrend:  []models.DailySeverityPoint{},
		HealthBuckets:  []models.HealthBucketStub{},
		ConfidenceDist: map[string]int{},
		Workflow:       models.WorkflowStub{Funnel: map[string]int{}},
	}
}

// sortedCategories renders a category→count map as a stable slice ordered by
// count desc then name, so the radar chart's axes don't reshuffle between requests.
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

// analyticsFromReports folds analysis reports into the confidence distribution
// and fix-availability tallies; missing confidence counts as the default (medium).
func analyticsFromReports(reports []*models.AnalysisReport) (confidence map[string]int, totalFindings, autoFixable int) {
	confidence = map[string]int{}
	for _, r := range reports {
		for _, f := range r.Findings {
			totalFindings++
			c := string(f.Confidence)
			if c == "" {
				c = string(models.ConfidenceMedium)
			}
			confidence[c]++
			if f.AutoFix != "" {
				autoFixable++
			}
		}
	}
	return confidence, totalFindings, autoFixable
}

// bucketHealth distributes health scores into five fixed 20-point buckets so
// the histogram chart exposes the shape the average conceals.
func bucketHealth(scores []int) []models.HealthBucketStub {
	buckets := []models.HealthBucketStub{
		{Label: "0-20", Lo: 0, Hi: 20},
		{Label: "20-40", Lo: 20, Hi: 40},
		{Label: "40-60", Lo: 40, Hi: 60},
		{Label: "60-80", Lo: 60, Hi: 80},
		{Label: "80-100", Lo: 80, Hi: 100},
	}
	for _, s := range scores {
		switch {
		case s < 20:
			buckets[0].Count++
		case s < 40:
			buckets[1].Count++
		case s < 60:
			buckets[2].Count++
		case s < 80:
			buckets[3].Count++
		default:
			buckets[4].Count++
		}
	}
	return buckets
}

// sortedRuleFreq renders a rule→count map as a stable slice ordered by count
// desc then name, capped at 15; severities tints bars, missing rules fall back in the UI.
func sortedRuleFreq(m map[string]int, severities map[string]string) []models.RuleFrequencyStub {
	out := make([]models.RuleFrequencyStub, 0, len(m))
	for rule, count := range m {
		out = append(out, models.RuleFrequencyStub{Rule: rule, Count: count, TopSeverity: severities[rule]})
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
