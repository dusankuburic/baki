package service

import (
	"context"
	"golang.org/x/sync/errgroup"
	"math"
	"sort"
	"strconv"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"
	"pad-analyzer/internal/platform/eventbus"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-core/cache"
	"pad-core/logger"
	"pad-core/models"
)

// dashboardCacheTTL bounds how long a stale dashboard result is served — the
// landing page triggers ~16 sequential DB queries, so 30s collapses re-loads.
const dashboardCacheTTL = 30 * time.Second

// dashboardHomeMax caps the home cache: one payload per user who opened the page within the TTL.
const dashboardHomeMax = 1024

// The dashboard's view caches (ADR 0017): each user's home payload, and each user's analytics payloads by query.
// An analytics payload also depends on whether the caller is an instance admin (the instance-wide security counts),
// so that flag is part of its key: two sessions of one user, with and without the admin role, never share a view.
// homeEpoch retires every home entry at once (the Version part).
var (
	homeViews = cache.Declare("dashboard.home",
		cache.Keys(cache.KindTenant, cache.KindVersion),
		cache.Shared(cache.MechanismInvalidation, "a flow change or analysis on any replica drops the owner's view on every replica once committed; the 30 s TTL bounds a lost message and other collaborators' views"))
	analyticsViews = cache.Declare("dashboard.analytics",
		cache.Keys(cache.KindTenant, cache.KindParam, cache.KindParam),
		cache.Shared(cache.MechanismInvalidation, "a flow change or analysis on any replica drops the views of the owner and of the flow's org on every replica once committed; the 30 s TTL bounds a lost message and other collaborators' views"))
)

// DashboardService assembles the welcome ("home") dashboard payload and persists per-flow analysis
// summaries — cloud reads the durable flow_analysis table, local (nil backend) the analyzer cache.
type DashboardService struct {
	backend  storageif.DashboardStore
	analysis *AnalysisService
	flowSvc  *FlowService

	// home holds each user's home payload (cloud) for the TTL; concurrent cold loads of one user share one
	// buildCloud run instead of each issuing ~16 DB queries. homeEpoch moves on a drop of every view.
	home      *cache.Typed[*models.DashboardHomeData]
	homeEpoch atomic.Uint64
	// ana holds analytics payloads keyed by user, admin flag and canonical query (dashboard_analytics.go).
	ana   *anaStore
	anaSF singleflight.Group
	scope ScopeResolver

	// homeGen fences home fills (keyed by user) and anaGen analytics fills (keyed "u:"+user and "o:"+org) against
	// the invalidations that ran while they read: a build that read the old rows is not stored after the drop.
	homeGen eventbus.Gen[string]
	anaGen  eventbus.Gen[string]
	// orgEpoch counts organisation invalidations: an analytics fill learns which organisations it spans only
	// from its scope read, so it captures their generations after that read and is refused if one ran meanwhile.
	orgEpoch atomic.Uint64

	// bus announces the drops that are not a FlowChanged's (an analysis recorded) to the other replicas; nil announces nothing.
	bus *eventbus.Bus
}

// DashboardOption configures NewDashboardService.
type DashboardOption func(*DashboardService)

// NewDashboardService builds the dashboard service; scope collects the analytics page's flow set (DI passes the
// library's FlowScopeResolver; without one BuildAnalytics fails).
func NewDashboardService(backend storageif.DashboardStore, analysis *AnalysisService, flowSvc *FlowService, scope ScopeResolver, opts ...DashboardOption) *DashboardService {
	s := &DashboardService{
		backend:  backend,
		analysis: analysis,
		flowSvc:  flowSvc,
		scope:    scope,
		home:     cache.New[*models.DashboardHomeData](homeViews, dashboardHomeMax, cache.WithTTL(dashboardCacheTTL)),
		ana:      newAnaStore(),
	}
	for _, o := range opts {
		o(s)
	}
	return s
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
		// The snapshot remembers the rules it was scored with, so the scanner compares a later health like for like.
		RuleSet: report.RuleSet,
	}
	if err := s.backend.SaveFlowAnalysis(ctx, fa); err != nil {
		logger.Warn("dashboard: persist flow analysis failed", "flowId", doc.ID, "err", err)
	}
	// The flow's summary changed (an analysis is not a change of the flow, so no FlowChanged): drop the views
	// built over it now instead of waiting out the TTL.
	s.InvalidateFlowViews(doc.OwnerID, doc.OrganizationID)
	// The other replicas' views of it too, once this write is durable.
	if scope, ok := flowViewScope(doc.OwnerID, doc.OrganizationID); ok {
		s.bus.Invalidate(ctx, "dashboard.home", scope)
		s.bus.Invalidate(ctx, "dashboard.analytics", scope)
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

	build := func(ctx context.Context) (*models.DashboardHomeData, int64, error) {
		out := s.buildCloud(ctx, userID)
		out.IsCloud = true
		return out, 1, nil
	}
	k, ok := s.homeKey(userID)
	if !ok {
		out, _, _ := build(ctx)
		return out
	}
	// Served from the cache for the TTL (16 queries → 0 on a hit) and stored even on partial failure — the
	// emptyHome stub is cheap to serve for 30s and avoids hammering the DB on a transient error — unless the
	// user's views were invalidated while it read. Do NOT mutate the result: it is a shared pointer handed to
	// every concurrent caller, and writing to it races their JSON marshal.
	out, err := s.home.GetOrFill(ctx, k, build, &s.homeGen, userID)
	if err != nil {
		// Only the caller's own context ending; fall back to a valid shape.
		return emptyHome()
	}
	return out
}

// homeKey is the home cache's key for userID under the current epoch; false without a user.
func (s *DashboardService) homeKey(userID string) (cache.Key, bool) {
	if userID == "" {
		return cache.Key{}, false
	}
	k, err := homeViews.Key(cache.User(userID), cache.Version(strconv.FormatUint(s.homeEpoch.Load(), 10)))
	return k, err == nil
}

// InvalidateDashboardCache clears the cached dashboard data for a user (all
// users when empty): their home and every analytics view they built.
func (s *DashboardService) InvalidateDashboardCache(userID string) {
	if userID != "" {
		s.InvalidateFlowViews(userID, "")
		return
	}
	// Generations first, so a fill in flight is refused, then every entry is retired.
	s.homeGen.BumpAll()
	s.anaGen.BumpAll()
	s.homeEpoch.Add(1)
	s.ana.clear()
}

// InvalidateFlowViews drops what a change to a flow of ownerID in orgID ("" for a personal flow) makes stale: the
// owner's home and analytics views, and every analytics view whose scope spans orgID, whoever built it. Other
// collaborators' views of a shared flow wait out the TTL: which users those are is not known without a read.
func (s *DashboardService) InvalidateFlowViews(ownerID, orgID string) {
	if ownerID == "" && orgID == "" {
		return
	}
	// Generations first, then the delete: a fill either sees the bump or stored before the delete removes it.
	if orgID != "" {
		s.orgEpoch.Add(1) // before the org's generation: see anaFence.scopeRead
		s.anaGen.Bump("o:" + orgID)
	}
	if ownerID != "" {
		s.homeGen.Bump(ownerID)
		s.anaGen.Bump("u:" + ownerID)
		if k, ok := s.homeKey(ownerID); ok {
			s.home.Delete(k)
		}
	}
	s.ana.dropViews(ownerID, orgID)
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
