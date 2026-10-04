package service

import (
	"container/list"
	"context"
	"fmt"
	"sort"
	"sync"
	"weak"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"golang.org/x/sync/semaphore"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/metrics"
	"pad-core/analyzer"
	"pad-core/logger"
	"pad-core/models"
)

// analysisReportBudget bounds the report pair cache by ESTIMATED BYTES (analyzer.ReportCost), not count.
const analysisReportBudget int64 = 96 << 20

// backgroundReportCeiling caps what a user-less caller may ADD to the pair cache — a sweep must not evict interactive entries.
const backgroundReportCeiling int64 = 16 << 20

// largeAnalysisBlocks is the block count from which an uncached analysis takes a concurrency slot.
const (
	largeAnalysisBlocks      = 20_000
	largeAnalysisConcurrency = 2
)

// largeAnalysisSem is the process-wide gate for large uncached analyses (shared by every instance).
var largeAnalysisSem = semaphore.NewWeighted(largeAnalysisConcurrency)

// analysisRunFunc is the uncached analysis entry point called after the cache probe and gate; replaceable in tests.
type analysisRunFunc func(ctx context.Context, doc *models.FlowDocument, rules []analyzer.Rule, settings *models.AppSettings, onProgress func(int, int, string)) *models.AnalysisReport

// reportPair tracks the two most recent distinct reports for a flow (for /api/analysis/diff).
type reportPair struct {
	prev    *models.AnalysisReport
	current *models.AnalysisReport
}

// cost is the bytes the pair charges against analysisReportBudget.
func (p *reportPair) cost() int64 {
	return analyzer.ReportCost(p.current) + analyzer.ReportCost(p.prev)
}

// dropPrev forgets the pair's previous report.
func (p *reportPair) dropPrev() {
	p.prev = nil
}

// recordClaim is AnalyzeFlowFresh's dashboard-record claim; matched by weak pointer while alive, else content identity.
type recordClaim struct {
	report   weak.Pointer[models.AnalysisReport]
	content  string
	findings int
	stats    models.AnalysisStats
}

func newRecordClaim(r *models.AnalysisReport) recordClaim {
	return recordClaim{report: weak.Make(r), content: r.ContentHash, findings: len(r.Findings), stats: r.Stats}
}

// matches reports whether r is the report this claim was taken for.
func (c recordClaim) matches(r *models.AnalysisReport) bool {
	if c.report.Value() == r {
		return true
	}
	// Reports from non-cached paths carry no ContentHash; only the pointer identifies them.
	return c.content != "" && c.content == r.ContentHash && c.findings == len(r.Findings) && c.stats == r.Stats
}

// maxRecordClaims bounds recordClaims by count (a claim is ~100 B).
const maxRecordClaims = 4096

// recordClaims is the count-bounded LRU of dashboard-record claims, keyed by the flow's history key.
type recordClaims struct {
	mu    sync.Mutex
	max   int
	order *list.List // front = most recently used
	index map[string]*list.Element
}

type recordClaimEntry struct {
	key   string
	claim recordClaim
}

func newRecordClaims(max int) *recordClaims {
	return &recordClaims{max: max, order: list.New(), index: make(map[string]*list.Element)}
}

// take claims r for key and reports whether it was free, so per-report side effects run exactly once.
func (c *recordClaims) take(key string, r *models.AnalysisReport) (fresh bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.index[key]; ok {
		e := el.Value.(*recordClaimEntry)
		if e.claim.matches(r) {
			c.order.MoveToFront(el)
			return false
		}
		c.order.Remove(el)
		delete(c.index, key)
	}
	for c.order.Len() >= c.max && c.order.Len() > 0 {
		back := c.order.Back()
		delete(c.index, back.Value.(*recordClaimEntry).key)
		c.order.Remove(back)
	}
	c.index[key] = c.order.PushFront(&recordClaimEntry{key: key, claim: newRecordClaim(r)})
	return true
}

// release gives the claim on key back if it names r; a no-op otherwise.
func (c *recordClaims) release(key string, r *models.AnalysisReport) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.index[key]; ok && el.Value.(*recordClaimEntry).claim.matches(r) {
		c.order.Remove(el)
		delete(c.index, key)
	}
}

// reportLRU is the byte-weighted LRU behind AnalysisService.reports; get-mutate-put is serialised by the service.
type reportLRU struct {
	mu     sync.Mutex
	budget int64
	bytes  int64
	order  *list.List // front = most recently used
	index  map[string]*list.Element
}

type reportLRUEntry struct {
	key  string
	pair *reportPair
	cost int64
}

func newReportLRU(budget int64) *reportLRU {
	return &reportLRU{budget: budget, order: list.New(), index: make(map[string]*list.Element)}
}

// Get returns the pair stored under key and marks it most recently used.
func (c *reportLRU) Get(key string) (*reportPair, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	el, ok := c.index[key]
	if !ok {
		return nil, false
	}
	c.order.MoveToFront(el)
	return el.Value.(*reportLRUEntry).pair, true
}

// Put stores pair under key, reporting retention: prev dropped if the pair exceeds the budget; LRU evicted by bytes.
func (c *reportLRU) Put(key string, pair *reportPair) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if el, ok := c.index[key]; ok {
		c.evict(el)
	}
	if analyzer.ReportCost(pair.current) > c.budget {
		return false
	}
	if pair.cost() > c.budget {
		pair.dropPrev()
	}
	cost := pair.cost()
	for c.bytes+cost > c.budget && c.order.Len() > 0 {
		c.evict(c.order.Back())
	}
	el := c.order.PushFront(&reportLRUEntry{key: key, pair: pair, cost: cost})
	c.index[key] = el
	c.bytes += cost
	return true
}

// evict removes el and refunds its cost. Caller holds c.mu.
func (c *reportLRU) evict(el *list.Element) {
	e := el.Value.(*reportLRUEntry)
	c.order.Remove(el)
	delete(c.index, e.key)
	c.bytes -= e.cost
}

// Len is the number of retained pairs.
func (c *reportLRU) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.order.Len()
}

// Bytes is the estimated bytes charged by the retained pairs.
func (c *reportLRU) Bytes() int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.bytes
}

// analysisHistoryKey returns a stable identity for trend/diff tracking (doc.ID is a fresh UUID per load).
func analysisHistoryKey(doc *models.FlowDocument) string {
	return analyzer.StableFlowID(doc)
}

type AnalysisService struct {
	notifier    EventNotifier
	settings    SettingsProvider
	history     *analyzer.HistoryStore
	customRules []analyzer.Rule
	// profiles resolves the per-org rule profile; nil (desktop/tests) falls back to deployment settings + custom rules.
	profiles *RuleProfileResolver
	// authEnabled: with JWT claims, EmitTo("") would broadcast to EVERY client — right only in desktop mode.
	authEnabled bool

	// largeSem gates uncached analyses of large flows (nil disables); run is the uncached entry point.
	largeSem *semaphore.Weighted
	run      analysisRunFunc

	mu      sync.Mutex
	reports *reportLRU
	claims  *recordClaims
}

// NewAnalysisService builds the service; the error return is kept for the DI signature.
func NewAnalysisService(notifier EventNotifier, settings SettingsProvider, history *analyzer.HistoryStore) (*AnalysisService, error) {
	return &AnalysisService{
		notifier: notifier,
		settings: settings,
		history:  history,
		largeSem: largeAnalysisSem,
		reports:  newReportLRU(analysisReportBudget),
		claims:   newRecordClaims(maxRecordClaims),
	}, nil
}

// SetRuleProfileResolver injects the per-org rule resolver; wired by DI in cloud mode, nil in local mode.
func (s *AnalysisService) SetRuleProfileResolver(r *RuleProfileResolver) {
	s.profiles = r
}

// SetCustomRules installs compiled deployment rules; DI hands the same slice to this service and the RuleProfileResolver.
func (s *AnalysisService) SetCustomRules(rules DeploymentCustomRules) {
	s.customRules = rules
}

// SetAuthEnabled records whether requests carry auth: with auth on, claim-less progress is dropped (not broadcast).
func (s *AnalysisService) SetAuthEnabled(enabled bool) {
	s.authEnabled = enabled
}

// AuthEnabled reports what SetAuthEnabled set.
func (s *AnalysisService) AuthEnabled() bool {
	return s.authEnabled
}

func (s *AnalysisService) LoadCustomRules(path string) {
	if path == "" {
		return
	}
	custom, warnings, err := analyzer.LoadCustomRules(path)
	if err != nil {
		// Don't fail boot — log and continue with built-in rules only.
		fmt.Printf("analysis service: custom rules load failed: %v\n", err)
		return
	}
	for _, w := range warnings {
		logger.Warn("custom rule skipped", "file", path, "detail", w)
	}
	s.customRules = custom
}

func (s *AnalysisService) AnalyzeFlow(ctx context.Context, doc *models.FlowDocument) (report *models.AnalysisReport, err error) {
	report, _, err = s.analyzeFlow(ctx, doc, analyzeOpts{record: true, progress: true})
	return report, err
}

// AnalyzeFlowFresh is AnalyzeFlow that also CLAIMS the dashboard record: fresh exactly once per report.
func (s *AnalysisService) AnalyzeFlowFresh(ctx context.Context, doc *models.FlowDocument) (report *models.AnalysisReport, fresh bool, err error) {
	return s.analyzeFlow(ctx, doc, analyzeOpts{record: true, progress: true, claim: true})
}

// ReleaseRecord gives back the dashboard-record claim for report; a no-op once the claim moved to a newer report.
func (s *AnalysisService) ReleaseRecord(doc *models.FlowDocument, report *models.AnalysisReport) {
	if s == nil || s.claims == nil || report == nil {
		return
	}
	s.claims.release(analysisHistoryKey(doc), report)
}

// AnalyzeFlowBackground analyzes and RECORDS but emits no progress; above backgroundReportCeiling nothing is ADDED.
func (s *AnalysisService) AnalyzeFlowBackground(ctx context.Context, doc *models.FlowDocument) (*models.AnalysisReport, error) {
	report, _, err := s.analyzeFlow(ctx, doc, analyzeOpts{record: true, background: true})
	return report, err
}

// AnalyzeFlowReadOnly analyzes WITHOUT recording or progress, so internal analyses cannot overwrite the diff pair.
func (s *AnalysisService) AnalyzeFlowReadOnly(ctx context.Context, doc *models.FlowDocument) (*models.AnalysisReport, error) {
	report, _, err := s.analyzeFlow(ctx, doc, analyzeOpts{})
	return report, err
}

// analyzeOpts selects what an analyzeFlow call records and emits (claim and background imply record).
type analyzeOpts struct {
	record     bool
	progress   bool
	claim      bool
	background bool
}

// analyzeFlow runs the cached analysis under opts; fresh reports whether a claim was still free.
func (s *AnalysisService) analyzeFlow(ctx context.Context, doc *models.FlowDocument, opts analyzeOpts) (report *models.AnalysisReport, fresh bool, err error) {
	defer logger.Guard("App.AnalyzeFlow", &err)

	record, progress, claim := opts.record, opts.progress, opts.claim

	if record {
		metrics.RecordAnalysisRun()
	}

	// Trace the run so its duration is attributable within the request trace; no-op without an OTLP exporter.
	_, span := otel.Tracer("analysis-service").Start(ctx, "AnalyzeFlow")
	defer span.End()

	if doc == nil {
		err = fmt.Errorf("no flow loaded")
		span.RecordError(err)
		return nil, false, err
	}

	// Rules run per the flow's OWNING ORG (pre-R4: process-global, one tenant's toggle changed every tenant).
	settings, rules := s.ruleSetFor(ctx, doc)

	// Extract userID for per-user event delivery (prevents cross-tenant leaks).
	userID := ""
	if claims := auth.ClaimsFromContext(ctx); claims != nil {
		userID = claims.UserID
	}

	// No callback when nobody is listening: a claim-less context with auth on would broadcast to every tenant.
	var onProgress func(current, total int, ruleName string)
	if progress && (userID != "" || !s.authEnabled) {
		onProgress = func(current, total int, ruleName string) {
			s.notifier.EmitTo(userID, "analysis:progress", map[string]any{
				"current":  current,
				"total":    total,
				"ruleName": ruleName,
			})
		}
	}
	// Probe the cache first so a hit never queues behind the large-analysis gate.
	result := analyzer.LookupCachedReport(doc, rules, settings)
	if result == nil {
		result, err = s.runGated(ctx, doc, rules, settings, onProgress)
		if err != nil {
			span.RecordError(err)
			return nil, false, err
		}
	}
	// nil with ctx cancelled: the truncated report is discarded inside the analyzer — surface the cancellation.
	if result == nil && ctx.Err() != nil {
		err = ctx.Err()
		span.RecordError(err)
		return nil, false, err
	}
	// Any other nil would be an analyzer bug; fail gracefully instead of panicking downstream.
	if result == nil {
		err = fmt.Errorf("analysis produced no result for flow %q", doc.ID)
		span.RecordError(err)
		return nil, false, err
	}

	// Surface skipped rules (panic recovery) so ops can alert when a rule silently produces no findings.
	if result.Stats.RulesSkipped > 0 {
		metrics.RecordRulesSkipped(result.Stats.RulesSkipped)
	}

	span.SetAttributes(
		attribute.String("flow.id", result.FlowID),
		attribute.Int("analysis.findings", len(result.Findings)),
	)

	// Track the two most recent distinct runs for diffing (pointer identity detects a new run).
	if record {
		key := analysisHistoryKey(doc)
		s.mu.Lock()
		pair, ok := s.reports.Get(key)
		if !ok {
			pair = &reportPair{}
		}
		newRun := pair.current != result
		if newRun {
			pair.prev = pair.current
			pair.current = result
		}
		// The claim is separate from the run tracking and lives outside the budgeted pair cache.
		if claim {
			fresh = s.claims.take(key, result)
		}
		// Re-put after mutating (the pair's cost changed); a background caller may not add a large one.
		if ok || !opts.background || analyzer.ReportCost(result) <= backgroundReportCeiling {
			s.reports.Put(key, pair)
		}
		s.mu.Unlock()
		if newRun && s.history != nil {
			s.history.Record(key, result, doc)
		}
	}

	// NOTE: no "analysis:complete" SSE broadcast — no client consumes it and it shipped megabytes.

	logger.Info("analysis complete",
		"flowId", result.FlowID,
		"findings", len(result.Findings),
		"durationMs", result.DurationMs,
	)

	return result, fresh, nil
}

// runGated runs the uncached analysis holding a large-analysis slot when large; released on every exit.
func (s *AnalysisService) runGated(ctx context.Context, doc *models.FlowDocument, rules []analyzer.Rule, settings *models.AppSettings, onProgress func(int, int, string)) (*models.AnalysisReport, error) {
	release, err := s.acquireLargeSlot(ctx, doc)
	if err != nil {
		return nil, err
	}
	defer release()
	run := s.run
	if run == nil {
		run = analyzer.CachedAnalysisCtx
	}
	return run(ctx, doc, rules, settings, onProgress), nil
}

// acquireLargeSlot takes a largeAnalysisSem slot for a large uncached flow; small flows get a no-op release.
func (s *AnalysisService) acquireLargeSlot(ctx context.Context, doc *models.FlowDocument) (release func(), err error) {
	if s.largeSem == nil || countBlocks(doc) < largeAnalysisBlocks {
		return func() {}, nil
	}
	if !s.largeSem.TryAcquire(1) {
		logger.Info("analysis waiting for a large-analysis slot", "flowId", doc.ID, "limit", largeAnalysisConcurrency)
		if err := s.largeSem.Acquire(ctx, 1); err != nil {
			return nil, err
		}
	}
	return func() { s.largeSem.Release(1) }, nil
}

// countBlocks is the block count, children included — a plain walk that relies on no transient index.
func countBlocks(doc *models.FlowDocument) int {
	n := 0
	for i := range doc.Subflows {
		n += countBlockTree(doc.Subflows[i].Blocks)
	}
	return n
}

func countBlockTree(blocks []models.Block) int {
	n := len(blocks)
	for i := range blocks {
		if len(blocks[i].Children) > 0 {
			n += countBlockTree(blocks[i].Children)
		}
	}
	return n
}

// TestRule runs ONE candidate rule and returns its findings — nothing cached, recorded or emitted.
func (s *AnalysisService) TestRule(ctx context.Context, doc *models.FlowDocument, cfg analyzer.CustomRuleConfig) ([]models.Finding, error) {
	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}
	rule, err := analyzer.NewCustomRule(cfg)
	if err != nil {
		return nil, fmt.Errorf("invalid rule: %w", err)
	}
	// Settings are nil: the org profile would let a DISABLED same-id rule silence the rule being tested.
	report := analyzer.RunAnalysisCtx(ctx, doc, []analyzer.Rule{rule}, nil, nil)
	// A cancelled walk skips the remaining blocks: its findings are a truncated subset.
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if report == nil {
		return nil, fmt.Errorf("analysis produced no result")
	}
	return report.Findings, nil
}

// PreviousReport returns the analysis run recorded immediately before the current one.
func (s *AnalysisService) PreviousReport(doc *models.FlowDocument) (*models.AnalysisReport, bool) {
	if s == nil || s.reports == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pair, _ := s.reports.Get(analysisHistoryKey(doc))
	if pair == nil || pair.prev == nil {
		return nil, false
	}
	return pair.prev, true
}

// CurrentReport returns the flow's most recent report; nil-safe on an unconfigured service.
func (s *AnalysisService) CurrentReport(doc *models.FlowDocument) (*models.AnalysisReport, bool) {
	if s == nil || s.reports == nil {
		return nil, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	pair, _ := s.reports.Get(analysisHistoryKey(doc))
	if pair == nil || pair.current == nil {
		return nil, false
	}
	return pair.current, true
}

// History returns the persisted analysis trend snapshots for a flow.
func (s *AnalysisService) History(doc *models.FlowDocument) []analyzer.AnalysisSnapshot {
	if s.history == nil {
		return nil
	}
	return s.history.Load(analysisHistoryKey(doc))
}

func (s *AnalysisService) GetVariableLineage(doc *models.FlowDocument, varName string) (history *models.VariableHistory, err error) {
	defer logger.Guard("App.GetVariableLineage", &err)

	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}

	return analyzer.BuildVariableLineage(doc, varName), nil
}

func (s *AnalysisService) GetExecutionGraph(doc *models.FlowDocument) (graph *models.GraphData, err error) {
	defer logger.Guard("App.GetExecutionGraph", &err)

	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}

	return analyzer.BuildExecutionGraph(doc, nil), nil
}

func (s *AnalysisService) GetDataFlow(doc *models.FlowDocument) (df *models.DataFlowAnalysis, err error) {
	defer logger.Guard("App.GetDataFlow", &err)

	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}

	return analyzer.AnalyzeDataFlow(doc), nil
}

func (s *AnalysisService) AnalyzeBatch(docs []*models.FlowDocument) (batch *models.BatchAnalysis, err error) {
	defer logger.Guard("App.AnalyzeBatch", &err)

	if len(docs) == 0 {
		return nil, fmt.Errorf("no flows provided")
	}

	settings := s.settings.Get()
	rules := analyzer.AllRules()
	if len(s.customRules) > 0 {
		rules = append(rules, s.customRules...)
	}
	return analyzer.RunBatchAnalysis(docs, rules, settings), nil
}

func (s *AnalysisService) DiffReports(old, new *models.AnalysisReport) *models.AnalysisDiff {
	return analyzer.DiffReports(old, new)
}

func (s *AnalysisService) GetDependencyAnalysis() *models.DependencyAnalysis {
	return analyzer.AnalyzeRuleDependencies()
}

func (s *AnalysisService) ComputeDashboard() *models.DashboardStats {
	return analyzer.ComputeDashboard(sortedReports(analyzer.DefaultCache.AllReports()))
}

// DashboardData returns stats and reports from one cache snapshot, so all sections see a consistent view.
func (s *AnalysisService) DashboardData() (*models.DashboardStats, []*models.AnalysisReport) {
	reports := sortedReports(analyzer.DefaultCache.AllReports())
	return analyzer.ComputeDashboard(reports), reports
}

// sortedReports orders a cache snapshot by FlowID (AllReports iterates a map).
func sortedReports(reports []*models.AnalysisReport) []*models.AnalysisReport {
	sort.Slice(reports, func(i, j int) bool { return reports[i].FlowID < reports[j].FlowID })
	return reports
}

func (s *AnalysisService) DeduplicateFindings(report *models.AnalysisReport) ([]models.Finding, []models.FindingGroup) {
	return analyzer.DeduplicateFindings(report.Findings)
}

func (s *AnalysisService) FindRelatedFindings(report *models.AnalysisReport, blockID string) []models.Finding {
	return analyzer.FindRelatedFindings(report.Findings, blockID)
}

func (s *AnalysisService) CompareFlows(docA, docB *models.FlowDocument) *models.FlowComparison {
	return analyzer.CompareFlows(docA, docB)
}

// GetRules returns the deployment-scoped rule catalog; prefer GetRulesFor in handlers so org members see the rules that actually run.
func (s *AnalysisService) GetRules() []models.Rule {
	return s.GetRulesFor(context.Background(), "")
}

// GetRulesFor returns the catalog as it applies to orgID — through the same resolver the analysis uses.
func (s *AnalysisService) GetRulesFor(ctx context.Context, orgID string) (result []models.Rule) {
	defer logger.GuardRecover("App.GetRules")

	var settings *models.AppSettings
	var rules []analyzer.Rule
	if orgID != "" && s.profiles != nil {
		profile := s.profiles.Resolve(ctx, orgID)
		settings, rules = profile.Settings, profile.Rules
	} else {
		if s.settings != nil {
			settings = s.settings.Get()
		}
		rules = analyzer.AllRules()
		if len(s.customRules) > 0 {
			rules = append(rules, s.customRules...)
		}
	}
	defaults := models.DefaultSettings()
	result = make([]models.Rule, len(rules))
	for i, r := range rules {
		result[i] = models.Rule{
			ID:              r.ID(),
			Name:            r.Name(),
			Description:     r.Description(),
			DefaultSeverity: r.DefaultSeverity(),
			Category:        r.Category(),
			Enabled:         true,
			Confidence:      analyzer.RuleConfidence(r.ID()),
			AutoFix:         analyzer.RuleAutoFix(r.ID()),
		}
		if settings != nil {
			if rc, ok := settings.Analysis.Rules[r.ID()]; ok {
				result[i].Enabled = rc.Enabled
				// Surface the configured severity override, else a later config save silently reverts it.
				if rc.Severity != "" {
					result[i].DefaultSeverity = models.Severity(rc.Severity)
				}
				if len(rc.Options) > 0 {
					result[i].Options = rc.Options
				}
			}
		}
		// Surface built-in threshold defaults for rules that carry them (reachable only by hand-editing settings JSON).
		if result[i].Options == nil {
			if d, ok := defaults.Analysis.Rules[r.ID()]; ok && len(d.Options) > 0 {
				result[i].Options = d.Options
			}
		}
	}
	return result
}

// GetRulesSummary folds the catalog into the dashboard's counts, reusing GetRules (same overrides).
func (s *AnalysisService) GetRulesSummary() models.RuleSummary {
	return s.GetRulesSummaryFor(context.Background(), "")
}

// GetRulesSummaryFor is GetRulesSummary over the org-scoped catalog.
func (s *AnalysisService) GetRulesSummaryFor(ctx context.Context, orgID string) models.RuleSummary {
	defer logger.GuardRecover("App.GetRulesSummary")

	summary := models.RuleSummary{
		ByCategory:   map[string]int{},
		ByConfidence: map[string]int{},
	}
	for _, r := range s.GetRulesFor(ctx, orgID) {
		summary.TotalRules++
		if r.AutoFix != "" {
			summary.AutoFixableRules++
		}
		if r.Category != "" {
			summary.ByCategory[r.Category]++
		}
		c := string(r.Confidence)
		if c == "" {
			c = string(models.ConfidenceMedium)
		}
		summary.ByConfidence[c]++
	}
	return summary
}

// SetRuleEnabled writes the DEPLOYMENT-WIDE settings singleton; in cloud mode the write joins the request's RLS transaction.
func (s *AnalysisService) SetRuleEnabled(ctx context.Context, ruleID string, enabled bool) (err error) {
	defer logger.Guard("App.SetRuleEnabled", &err)

	settings := s.settings.Get()

	if settings.Analysis.Rules == nil {
		settings.Analysis.Rules = make(map[string]models.RuleConfig)
	}

	rc := settings.Analysis.Rules[ruleID]
	rc.Enabled = enabled
	settings.Analysis.Rules[ruleID] = rc

	return s.settings.Update(ctx, *settings)
}

// UpdateRuleConfig writes the deployment-wide settings singleton; see SetRuleEnabled for the context.
func (s *AnalysisService) UpdateRuleConfig(ctx context.Context, ruleID string, config models.RuleConfig) (err error) {
	defer logger.Guard("App.UpdateRuleConfig", &err)

	settings := s.settings.Get()

	if settings.Analysis.Rules == nil {
		settings.Analysis.Rules = make(map[string]models.RuleConfig)
	}

	// Merge semantics: clients sending only {enabled, severity} must not wipe configured thresholds (e.g. maxDepth).
	if config.Options == nil {
		config.Options = settings.Analysis.Rules[ruleID].Options
	}
	settings.Analysis.Rules[ruleID] = config

	return s.settings.Update(ctx, *settings)
}
