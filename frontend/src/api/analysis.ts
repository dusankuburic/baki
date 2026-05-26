import {request} from './client'
import type {AnalysisReport, VariableHistory, GraphData, Rule, RuleConfig} from '@/types/domain'

export const analysisApi = {
  analyzeFlow: (): Promise<AnalysisReport> =>
    request('/api/analysis/analyze'),

  getVariableLineage: (varName: string): Promise<VariableHistory> =>
    request('/api/analysis/lineage', {varName}),

  getExecutionGraph: (): Promise<GraphData> =>
    request('/api/analysis/graph'),

  getRules: (): Promise<Rule[]> =>
    request('/api/analysis/rules'),

  updateRuleConfig: (ruleId: string, config: RuleConfig): Promise<void> =>
    request('/api/analysis/rule/config', {ruleId, config}),

  setRuleEnabled: (ruleId: string, enabled: boolean): Promise<void> =>
    request('/api/analysis/rule/enabled', {ruleId, enabled}),
}
