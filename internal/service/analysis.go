package service

import (
	"context"
	"fmt"
	"sync"

	"pad-analyzer/internal/analyzer"
	"pad-analyzer/internal/logger"
	"pad-analyzer/internal/models"
	"pad-analyzer/internal/storage"
)

// AnalysisService owns analysis report state and all analysis-related operations.
type AnalysisService struct {
	ctx        context.Context
	notifier   Notifier
	flow       *FlowService
	settings   *storage.SettingsStore
	lastReport *models.AnalysisReport
	reportMu   sync.Mutex
}

func NewAnalysisService(ctx context.Context, notifier Notifier, flow *FlowService, settings *storage.SettingsStore) *AnalysisService {
	return &AnalysisService{ctx: ctx, notifier: notifier, flow: flow, settings: settings}
}

// LastReport returns the last analysis report under a mutex lock.
func (s *AnalysisService) LastReport() *models.AnalysisReport {
	s.reportMu.Lock()
	defer s.reportMu.Unlock()
	return s.lastReport
}

func (s *AnalysisService) AnalyzeFlow() (report *models.AnalysisReport, err error) {
	defer logger.Guard("App.AnalyzeFlow", &err)

	curDoc := s.flow.CurrentDoc()

	if curDoc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}

	settings := s.settings.Get()

	rules := analyzer.AllRules()

	result := analyzer.RunAnalysis(curDoc, rules, settings, func(current, total int, ruleName string) {
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

	s.reportMu.Lock()
	s.lastReport = result
	s.reportMu.Unlock()

	return result, nil
}

func (s *AnalysisService) GetVariableLineage(varName string) (history *models.VariableHistory, err error) {
	defer logger.Guard("App.GetVariableLineage", &err)

	curDoc := s.flow.CurrentDoc()

	if curDoc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}

	return analyzer.BuildVariableLineage(curDoc, varName), nil
}

func (s *AnalysisService) GetExecutionGraph() (graph *models.GraphData, err error) {
	defer logger.Guard("App.GetExecutionGraph", &err)

	curDoc := s.flow.CurrentDoc()

	if curDoc == nil {
		return nil, fmt.Errorf("no flow loaded")
	}

	s.reportMu.Lock()
	report := s.lastReport
	s.reportMu.Unlock()

	return analyzer.BuildExecutionGraph(curDoc, report), nil
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
