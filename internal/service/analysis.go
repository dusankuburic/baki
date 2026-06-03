package service

import (
	"context"
	"fmt"

	"pad-analyzer/internal/analyzer"
	"pad-analyzer/internal/logger"
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
type AnalysisService struct {
	notifier Notifier
	settings *storage.SettingsStore
}

func NewAnalysisService(notifier Notifier, settings *storage.SettingsStore) *AnalysisService {
	return &AnalysisService{notifier: notifier, settings: settings}
}

func (s *AnalysisService) AnalyzeFlow(ctx context.Context, doc *models.FlowDocument) (report *models.AnalysisReport, err error) {
	defer logger.Guard("App.AnalyzeFlow", &err)

	if doc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}

	settings := s.settings.Get()
	rules := analyzer.AllRules()

	result := analyzer.RunAnalysis(doc, rules, settings, func(current, total int, ruleName string) {
		s.notifier.Emit("analysis:progress", map[string]any{
			"current":  current,
			"total":    total,
			"ruleName": ruleName,
		})
	})

	s.notifier.Emit("analysis:complete", result)

	logger.Info("analysis complete",
		"flowId", result.FlowID,
		"findings", len(result.Findings),
		"durationMs", result.DurationMs,
	)

	return result, nil
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

	// In stateless mode, we don't cache reports. BuildExecutionGraph can
	// work without a report (it just won't have finding markers on nodes).
	return analyzer.BuildExecutionGraph(doc, nil), nil
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

	settings.Analysis.Rules[ruleID] = config

	return s.settings.Update(*settings)
}
