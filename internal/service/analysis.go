package service

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"sync"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"

	"pad-analyzer/internal/analyzer"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/models"
	"pad-analyzer/internal/storage"
)

// AnalysisService owns analysis report state and all analysis-related operations.
//
// The analysis methods operate on an explicitly supplied *models.FlowDocument
// (resolved + authorized by the caller), so the service holds no global
// "current document". Reports are cached per flow id so the execution graph can
// reuse the matching report. lastReport tracks the most recent analysis purely
// to enrich chat context (which is not yet per-flow in cloud mode).
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
// the exact workflow diffing exists for. Path-less docs (cloud library docs,
// uploads) keep their persistent IDs.
func analysisHistoryKey(doc *models.FlowDocument) string {
	if doc.FilePath != "" {
		sum := sha256.Sum256([]byte(doc.FilePath))
		return "path-" + hex.EncodeToString(sum[:8])
	}
	return doc.ID
}

type AnalysisService struct {
	notifier Notifier
	settings *storage.SettingsStore
	history  *analyzer.HistoryStore

	mu      sync.Mutex
	reports map[string]*reportPair
}

func NewAnalysisService(notifier Notifier, settings *storage.SettingsStore, history *analyzer.HistoryStore) *AnalysisService {
	return &AnalysisService{
		notifier: notifier,
		settings: settings,
		history:  history,
		reports:  make(map[string]*reportPair),
	}
}

func (s *AnalysisService) AnalyzeFlow(ctx context.Context, doc *models.FlowDocument) (report *models.AnalysisReport, err error) {
	defer logger.Guard("App.AnalyzeFlow", &err)

	metrics.RecordAnalysisRun()

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

	settings := s.settings.Get()
	rules := analyzer.AllRules()

	// Extract userID for per-user event delivery (prevents cross-tenant leaks).
	userID := ""
	if claims := auth.ClaimsFromContext(ctx); claims != nil {
		userID = claims.UserID
	}

	result := analyzer.CachedAnalysis(doc, rules, settings, func(current, total int, ruleName string) {
		s.notifier.EmitTo(userID, "analysis:progress", map[string]any{
			"current":  current,
			"total":    total,
			"ruleName": ruleName,
		})
	})

	span.SetAttributes(
		attribute.String("flow.id", result.FlowID),
		attribute.Int("analysis.findings", len(result.Findings)),
	)

	// Track the two most recent distinct runs for diffing, and record a trend
	// snapshot. Pointer identity detects freshness: cache hits return the same
	// *AnalysisReport, so repeated analyzes of unchanged content are no-ops.
	key := analysisHistoryKey(doc)
	s.mu.Lock()
	pair := s.reports[key]
	if pair == nil {
		pair = &reportPair{}
		s.reports[key] = pair
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

	s.notifier.EmitTo(userID, "analysis:complete", result)

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
func (s *AnalysisService) PreviousReport(doc *models.FlowDocument) (*models.AnalysisReport, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	pair := s.reports[analysisHistoryKey(doc)]
	if pair == nil || pair.prev == nil {
		return nil, false
	}
	return pair.prev, true
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
	return analyzer.RunBatchAnalysis(docs, rules, settings), nil
}

func (s *AnalysisService) DiffReports(old, new *models.AnalysisReport) *models.AnalysisDiff {
	return analyzer.DiffReports(old, new)
}

func (s *AnalysisService) GenerateHTMLReport(report *models.AnalysisReport) string {
	return analyzer.GenerateHTMLReport(report)
}

func (s *AnalysisService) GenerateBatchHTMLReport(batch *models.BatchAnalysis) string {
	return analyzer.GenerateBatchHTMLReport(batch)
}

func (s *AnalysisService) GetDependencyAnalysis() *models.DependencyAnalysis {
	return analyzer.AnalyzeRuleDependencies()
}

func (s *AnalysisService) ComputeDashboard() *models.DashboardStats {
	reports := analyzer.DefaultCache.AllReports()
	return analyzer.ComputeDashboard(reports)
}

// CachedReports returns all analysis reports currently in the session cache.
// Used by the local-mode dashboard to populate complexity and rule frequency
// cards without a Postgres backend.
func (s *AnalysisService) CachedReports() []*models.AnalysisReport {
	return analyzer.DefaultCache.AllReports()
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
	result = make([]models.Rule, len(rules))
	for i, r := range rules {
		result[i] = models.Rule{
			ID:              r.ID(),
			Name:            r.Name(),
			Description:     r.Description(),
			DefaultSeverity: r.DefaultSeverity(),
			Category:        r.Category(),
			Enabled:         true,
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
