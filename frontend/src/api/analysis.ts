import {request} from './client'
import {useFlowStore} from '@/stores/flowStore'
import type {AnalysisReport, VariableHistory, GraphData, Rule, RuleConfig} from '@/types/domain'

// activeFlowId returns the id of the currently loaded flow. The backend ignores
// it in local/desktop mode (it operates on the in-memory document) and requires
// it in cloud mode to identify + authorize the target flow.
function activeFlowId(): string | undefined {
  return useFlowStore.getState().document?.id
}

export const analysisApi = {
  analyzeFlow: (): Promise<AnalysisReport> =>
    request('/api/analysis/analyze', {flowId: activeFlowId()}),

  getVariableLineage: (varName: string): Promise<VariableHistory> =>
    request('/api/analysis/lineage', {flowId: activeFlowId(), varName}),

  getExecutionGraph: (): Promise<GraphData> =>
    request('/api/analysis/graph', {flowId: activeFlowId()}),

  getRules: (): Promise<Rule[]> =>
    request('/api/analysis/rules'),

  updateRuleConfig: (ruleId: string, config: RuleConfig): Promise<void> =>
    request('/api/analysis/rule/config', {ruleId, config}),

  setRuleEnabled: (ruleId: string, enabled: boolean): Promise<void> =>
    request('/api/analysis/rule/enabled', {ruleId, enabled}),
}
