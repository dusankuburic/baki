import {request} from './client'
import {useFlowStore} from '@/stores/flowStore'
import type {AnalysisReport, VariableHistory, GraphData, Rule, RuleConfig, FlowMetrics, DataFlowAnalysis, AnalysisSnapshot, BatchAnalysis, AnalysisDiff, DependencyAnalysis, DashboardStats, SubflowHash, DeduplicateResult, FlowComparison, Finding} from '@/types'

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

  getMetrics: (): Promise<FlowMetrics> =>
    request('/api/analysis/metrics', {flowId: activeFlowId()}),

  getDataFlow: (): Promise<DataFlowAnalysis> =>
    request('/api/analysis/dataflow', {flowId: activeFlowId()}),

  getRules: (): Promise<Rule[]> =>
    request('/api/analysis/rules', undefined, 'GET'),

  updateRuleConfig: (ruleId: string, config: RuleConfig): Promise<void> =>
    request('/api/analysis/rule/config', {ruleId, config}),

  setRuleEnabled: (ruleId: string, enabled: boolean): Promise<void> =>
    request('/api/analysis/rule/enabled', {ruleId, enabled}),

  getHistory: (): Promise<AnalysisSnapshot[]> =>
    request('/api/analysis/history', {flowId: activeFlowId()}),

  batchAnalyze: (folderPath: string): Promise<BatchAnalysis> =>
    request('/api/analysis/batch', {folderPath}),

  getDiff: (): Promise<AnalysisDiff> =>
    request('/api/analysis/diff', {flowId: activeFlowId()}),

  exportHTML: (): Promise<string> =>
    request('/api/analysis/export/html', {flowId: activeFlowId()}),

  getDependencies: (): Promise<DependencyAnalysis> =>
    request('/api/analysis/dependencies', undefined, 'GET'),

  getDashboard: (): Promise<DashboardStats> =>
    request('/api/analysis/dashboard', undefined, 'GET'),

  getSubflowHashes: (): Promise<SubflowHash[]> =>
    request('/api/analysis/subflow-hashes', {flowId: activeFlowId()}),

  deduplicate: (): Promise<DeduplicateResult> =>
    request('/api/analysis/deduplicate', {flowId: activeFlowId()}),

  getRelatedFindings: (blockId: string): Promise<Finding[]> =>
    request('/api/analysis/related', {flowId: activeFlowId(), blockId}),

  compareFlows: (flowAId: string, flowBId: string): Promise<FlowComparison> =>
    request('/api/analysis/compare', {flowAId, flowBId}),
}
