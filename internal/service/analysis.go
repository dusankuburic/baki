package service

import (
	"context"
	"fmt"
	"sort"
	"sync"

	lru "github.com/hashicorp/golang-lru/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/metrics"
	"pad-core/analyzer"
	"pad-core/logger"
	"pad-core/models"
)

// maxAnalysisReports bounds the in-memory report cache so it can't grow
// unbounded over the process lifetime. Each entry holds two full
// AnalysisReports (prev + current) with their complete findings slices, so
// capping prevents OOM on long-running cloud instances. 50 matches the
// analyzer's own DefaultCache size.
const maxAnalysisReports = 50

// AnalysisService owns analysis report state and all analysis-related operations.
//
// The analysis methods operate on an explicitly supplied *models.FlowDocument
// (resolved + authorized by the caller), so the service holds no global
// "current document". Reports are cached per flow id so the execution graph can
// reuse the matching report. CurrentReport exposes the cached report for a flow
// so chat context can be grounded per-flow (StreamChatMessage resolves it when
// the handler passes nil) without re-analyzing on every chat turn.
// reportPair tracks the two most recent distinct analysis reports for a flow
// so /api/analysis/diff can compare runs (previously it diffed against an
// empty report, so everything was always "added").
type reportPair struct {
	prev    *models.AnalysisReport
	current *models.AnalysisReport
}

// analysisHistoryKey returns a stable identity for trend/diff tracking. The
// parser mints a fresh doc UUID on every load, so keying by doc.ID would
// fragment history each time the user reopens (or edits + reloads) a file —
// the exact workflow diffing exists for. Delegates to the analyzer's
// StableFlowID so history/diff and the session analytics cache agree on
// identity (byte-identical keys).
func analysisHistoryKey(doc *models.FlowDocument) string {
	return analyzer.StableFlowID(doc)
}

type AnalysisService struct {
	notifier    EventNotifier
	settings    SettingsProvider
	history     *analyzer.HistoryStore
	customRules []analyzer.Rule
	// profiles resolves the per-org rule profile (which rules run, at what
	// severity). Nil in desktop/local mode and in tests that don't need it, in
	// which case analysis falls back to the deployment settings + the
	// file-loaded custom rules — the pre-R4 behaviour.
	profiles *RuleProfileResolver

	mu      sync.Mutex
	reports *lru.Cache[string, *reportPair]
}

func NewAnalysisService(notifier EventNotifier, settings SettingsProvider, history *analyzer.HistoryStore) (*AnalysisService, error) {
	cache, err := lru.New[string, *reportPair](maxAnalysisReports)
	if err != nil {
		// Surface the error to the caller (fx fails boot cleanly) instead of
		// panicking mid-startup. In practice maxAnalysisReports is a >0 const
		// and lru.New never errors, but constructors shouldn't panic.
		return nil, fmt.Errorf("analysis reports LRU: %w", err)
	}
	return &AnalysisService{
		notifier: notifier,
		settings: settings,
		history:  history,
		reports:  cache,
	}, nil
}

// LoadCustomRules loads user-defined rules from a JSON file (see
// core/analyzer/custom_rules.go for the format). Called once at construction;
// custom rules are folded into every subsequent analysis run alongside the
// built-in rules. Invalid entries are skipped with a startup WARNING each (a
// silent skip used to leave operators believing a typo'd rule was enforcing);
// a missing file is a no-op.
// SetRuleProfileResolver injects the per-org rule resolver. Wired by DI in
// cloud mode; left nil in local mode where there are no orgs. Call before the
// service serves traffic.
func (s *AnalysisService) SetRuleProfileResolver(r *RuleProfileResolver) {
	s.profiles = r
}

// SetCustomRules installs already-compiled deployment rules. DI loads the file
// once (ProvideDeploymentCustomRules) and hands the same slice to both this
// service and the RuleProfileResolver, so the two cannot disagree about the
// deployment layer.
func (s *AnalysisService) SetCustomRules(rules DeploymentCustomRules) {
	s.customRules = rules
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
	return s.analyzeFlow(ctx, doc, true)
}

// AnalyzeFlowReadOnly analyzes WITHOUT recording into the report-history pair
// or the trend store, and without progress events or run metrics. The chat
// tool loop runs it against its working copy of the document (and against the
// real doc for fix tools) — those internal analyses used to overwrite
// CurrentReport / the diff pair (same StableFlowID for scrubbed clones) and
// flash the UI's analysis progress bar mid-chat.
func (s *AnalysisService) AnalyzeFlowReadOnly(ctx context.Context, doc *models.FlowDocument) (*models.AnalysisReport, error) {
	return s.analyzeFlow(ctx, doc, false)
}

func (s *AnalysisService) analyzeFlow(ctx context.Context, doc *models.FlowDocument, record bool) (report *models.AnalysisReport, err error) {
	defer logger.Guard("App.AnalyzeFlow", &err)

	if record {
		metrics.RecordAnalysisRun()
	}

	// Trace the analysis run so its (potentially long, CPU-bound) duration is
	// attributable within the request trace. No-op when no OTLP exporter is
	// configured. AI calls are traced separately in ai/tracing.go.
	_, span := otel.Tracer("analysis-service").Start(ctx, "AnalyzeFlow")
	defer span.End()

	if doc == nil {
		err = fmt.Errorf("no flow loaded")
		span.RecordError(err)
		return nil, err
	}

	// Which rules run, and at what severity, is resolved for the flow's OWNING
	// ORG. Before R4 both came from process-global state, so one tenant's rule
	// toggle changed analysis for every other tenant in the deployment.
	//
	// The resolved rule set feeds analyzer.CachedAnalysisCtx below, whose key
	// includes a digest of that set (analyzer.ruleSetDigest) — without it two
	// orgs whose flows share a name and content would share a cached report and
	// one would receive the other's custom-rule findings.
	var settings *models.AppSettings
	var rules []analyzer.Rule
	if s.profiles != nil {
		profile := s.profiles.Resolve(ctx, doc.OrganizationID)
		settings, rules = profile.Settings, profile.Rules
	} else {
		settings = s.settings.Get()
		rules = analyzer.AllRules()
		if len(s.customRules) > 0 {
			rules = append(rules, s.customRules...)
		}
	}

	// Extract userID for per-user event delivery (prevents cross-tenant leaks).
	userID := ""
	if claims := auth.ClaimsFromContext(ctx); claims != nil {
		userID = claims.UserID
	}

	onProgress := func(current, total int, ruleName string) {}
	if record {
		onProgress = func(current, total int, ruleName string) {
			s.notifier.EmitTo(userID, "analysis:progress", map[string]any{
				"current":  current,
				"total":    total,
				"ruleName": ruleName,
			})
		}
	}
	result := analyzer.CachedAnalysisCtx(ctx, doc, rules, settings, onProgress)
	// CachedAnalysis never returns nil today, but a future hash-miss / edge case
	// could; fail gracefully instead of letting a nil result panic the API
	// process when the downstream code dereferences it (span attrs, history,
	// logging). Resolves the inconsistency where result was nil-checked on the
	// skipped-rules line but unconditionally dereferenced everywhere else.
	if result == nil {
		err = fmt.Errorf("analysis produced no result for flow %q", doc.ID)
		span.RecordError(err)
		return nil, err
	}

	// Surface skipped rules (safeCheck panic recovery) on a Prometheus counter
	// so ops can alert when a rule silently produces no findings for a flow.
	if result.Stats.RulesSkipped > 0 {
		metrics.RecordRulesSkipped(result.Stats.RulesSkipped)
	}

	span.SetAttributes(
		attribute.String("flow.id", result.FlowID),
		attribute.Int("analysis.findings", len(result.Findings)),
	)

	// Track the two most recent distinct runs for diffing, and record a trend
	// snapshot — recording runs only (see AnalyzeFlowReadOnly). Pointer
	// identity detects freshness: cache hits return the same
	// *AnalysisReport, so repeated analyzes of unchanged content are no-ops.
	if record {
		key := analysisHistoryKey(doc)
		s.mu.Lock()
		pair, ok := s.reports.Get(key)
		if !ok {
			pair = &reportPair{}
			s.reports.Add(key, pair)
		}
		fresh := pair.current != result
		if fresh {
			pair.prev = pair.current
			pair.current = result
		}
		s.mu.Unlock()
		if fresh && s.history != nil {
			s.history.Record(key, result, doc)
		}
	}

	// NOTE: we intentionally do NOT broadcast the full report over SSE here.
	// AnalyzeFlow already returns the report to the caller via the HTTP response,
	// and no client consumes an "analysis:complete" event. Emitting it shipped the
	// entire report (megabytes on large flows) over the SSE bus only to be parsed
	// on the webview main thread and discarded — a needless UI-thread stall right
	// as analysis finished. Progress is still streamed via "analysis:progress".

	logger.Info("analysis complete",
		"flowId", result.FlowID,
		"findings", len(result.Findings),
		"durationMs", result.DurationMs,
	)

	return result, nil
}

// PreviousReport returns the analysis run recorded immediately before the
// current one for the flow, or (nil, false) when only zero or one distinct
// runs have happened.
// TestRule runs ONE candidate rule against a document and returns just its
// findings. Nothing is cached, recorded, or emitted.
//
// It exists because "the rule compiles" and "the rule does anything" are
// different questions, and only the first was answerable before: an author
// could save a regex that never matches and believe the org was protected. That
// is the same failure R1-5's suppression inventory was built to expose — a
// directive that silently masks nothing.
//
// Deliberately NOT routed through CachedAnalysisCtx: a candidate rule is not
// part of any org's profile, so caching its result would put an entry keyed on
// a rule set that no analysis will ever request again.
func (s *AnalysisService) TestRule(ctx context.Context, doc *models.FlowDocument, cfg analyzer.CustomRuleConfig) ([]models.Finding, error) {
	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}
	rule, err := analyzer.NewCustomRule(cfg)
	if err != nil {
		return nil, fmt.Errorf("invalid rule: %w", err)
	}
	// Settings are nil: a candidate rule has no configured enable/severity
	// entry, and passing the org profile would let a DISABLED same-id rule
	// silence the very thing being tested.
	report := analyzer.RunAnalysisCtx(ctx, doc, []analyzer.Rule{rule}, nil, nil)
	if report == nil {
		return nil, fmt.Errorf("analysis produced no result")
	}
	return report.Findings, nil
}

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

// CurrentReport returns the most recent analysis report for the flow, or
// (nil, false) when no analysis has been run yet. Nil-safe on an unconfigured
// service (a bare struct literal with no LRU, as some tests construct) so
// callers like the chat fallback can call it without a separate nil-cache guard.
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

func (s *AnalysisService) GetFlowMetrics(doc *models.FlowDocument) (metrics *models.FlowMetrics, err error) {
	defer logger.Guard("App.GetFlowMetrics", &err)

	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}

	return analyzer.ComputeFlowMetrics(doc, nil), nil
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

func (s *AnalysisService) GenerateBatchHTMLReport(batch *models.BatchAnalysis) string {
	return analyzer.GenerateBatchHTMLReport(batch)
}

func (s *AnalysisService) GetDependencyAnalysis() *models.DependencyAnalysis {
	return analyzer.AnalyzeRuleDependencies()
}

func (s *AnalysisService) ComputeDashboard() *models.DashboardStats {
	return analyzer.ComputeDashboard(sortedReports(analyzer.DefaultCache.AllReports()))
}

// DashboardData returns aggregated stats and per-flow reports from a single
// cache snapshot. The single-snapshot guarantee is important for the
// local-mode dashboard so all sections (overview, findings, complexity
// scatter) see a consistent view — two separate reads of the cache can
// diverge if an analysis completes between them. The cache holds one entry
// per stable flow identity (Put replaces prior hashes and evicts overlapping
// folder/file paths), so no dedup is needed here.
func (s *AnalysisService) DashboardData() (*models.DashboardStats, []*models.AnalysisReport) {
	reports := sortedReports(analyzer.DefaultCache.AllReports())
	return analyzer.ComputeDashboard(reports), reports
}

// sortedReports orders a cache snapshot by FlowID so dashboard sections render
// deterministically (AllReports iterates a map).
func sortedReports(reports []*models.AnalysisReport) []*models.AnalysisReport {
	sort.Slice(reports, func(i, j int) bool { return reports[i].FlowID < reports[j].FlowID })
	return reports
}

func (s *AnalysisService) ComputeSubflowHashes(doc *models.FlowDocument) []models.SubflowHash {
	return analyzer.ComputeSubflowHashes(doc)
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

func (s *AnalysisService) GetRules() (result []models.Rule) {
	defer logger.GuardRecover("App.GetRules")

	var settings *models.AppSettings
	if s.settings != nil {
		settings = s.settings.Get()
	}

	rules := analyzer.AllRules()
	if len(s.customRules) > 0 {
		rules = append(rules, s.customRules...)
	}
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
				// Surface the configured severity override; otherwise the UI
				// shows the built-in default and a subsequent config save
				// silently reverts the user's override.
				if rc.Severity != "" {
					result[i].DefaultSeverity = models.Severity(rc.Severity)
				}
			}
		}
	}
	return result
}

// GetRulesSummary folds the rule catalog (after applying user overrides) into
// the counts the dashboard needs: total rules, how many are auto-fixable, and
// the distribution across categories and confidence tiers. Reuses GetRules so
// the summary honours the same enabled/severity overrides as the catalog.
func (s *AnalysisService) GetRulesSummary() models.RuleSummary {
	defer logger.GuardRecover("App.GetRulesSummary")

	summary := models.RuleSummary{
		ByCategory:   map[string]int{},
		ByConfidence: map[string]int{},
	}
	for _, r := range s.GetRules() {
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

func (s *AnalysisService) SetRuleEnabled(ruleID string, enabled bool) (err error) {
	defer logger.Guard("App.SetRuleEnabled", &err)

	settings := s.settings.Get()

	if settings.Analysis.Rules == nil {
		settings.Analysis.Rules = make(map[string]models.RuleConfig)
	}

	rc := settings.Analysis.Rules[ruleID]
	rc.Enabled = enabled
	settings.Analysis.Rules[ruleID] = rc

	return s.settings.Update(*settings)
}

func (s *AnalysisService) UpdateRuleConfig(ruleID string, config models.RuleConfig) (err error) {
	defer logger.Guard("App.UpdateRuleConfig", &err)

	settings := s.settings.Get()

	if settings.Analysis.Rules == nil {
		settings.Analysis.Rules = make(map[string]models.RuleConfig)
	}

	// Merge semantics: clients sending only {enabled, severity} must not wipe
	// configured rule thresholds (e.g. deep-nesting maxDepth).
	if config.Options == nil {
		config.Options = settings.Analysis.Rules[ruleID].Options
	}
	settings.Analysis.Rules[ruleID] = config

	return s.settings.Update(*settings)
}
