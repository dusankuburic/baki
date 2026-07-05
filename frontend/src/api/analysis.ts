import {request} from './client'
import {useFlowStore} from '@/stores/flowStore'

// Folder-wide batch analysis can run rules over many flows, well beyond the
// default request timeout.
const BATCH_ANALYSIS_TIMEOUT_MS = 300_000
import type {AnalysisReport, VariableHistory, GraphData, Rule, RuleConfig, FlowMetrics, DataFlowAnalysis, AnalysisSnapshot, BatchAnalysis, AnalysisDiff, DependencyAnalysis, DashboardStats, SubflowHash, DeduplicateResult, FlowComparison, Finding, FindingStatus, FlowBaseline, BaselineDrift, TriageStatus, FindingComment} from '@/types'

// Input for setFindingStatus. flowId defaults to the active flow.
export interface SetFindingStatusInput {
  findingKey: string
  status: TriageStatus
  ruleId?: string
  justification?: string
  assigneeId?: string
  flowId?: string
}

// One item of a batch finding-status update (flowId is shared across the batch).
export interface BatchFindingStatusItem {
  findingKey: string
  status: TriageStatus
  ruleId?: string
  justification?: string
  assigneeId?: string
}

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
    request('/api/analysis/batch', {folderPath}, 'POST', BATCH_ANALYSIS_TIMEOUT_MS),

  getDiff: (): Promise<AnalysisDiff> =>
    request('/api/analysis/diff', {flowId: activeFlowId()}),

  exportHTML: (): Promise<string> =>
    request('/api/analysis/export/html', {flowId: activeFlowId()}),

  exportSARIF: (flowId: string = activeFlowId() ?? ''): Promise<unknown> =>
    request('/api/analysis/export/sarif', {flowId}),

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

  // --- Finding triage & baselines (persistent, team-shared; cloud mode only) ---

  listFindingStatuses: (flowId: string = activeFlowId() ?? ''): Promise<FindingStatus[]> =>
    request('/api/analysis/triage/list', {flowId}),

  setFindingStatus: (input: SetFindingStatusInput): Promise<FindingStatus> =>
    request('/api/analysis/triage/set', {flowId: activeFlowId(), ...input}),

  // Apply the same status to many findings of one flow in a single request.
  setFindingStatusBatch: (items: BatchFindingStatusItem[], flowId: string = activeFlowId() ?? ''): Promise<{updated: number}> =>
    request('/api/analysis/triage/set-batch', {flowId, items}),

  clearFindingStatus: (findingKey: string, flowId: string = activeFlowId() ?? ''): Promise<void> =>
    request('/api/analysis/triage/clear', {flowId, findingKey}),

  getBaseline: (flowId: string = activeFlowId() ?? ''): Promise<FlowBaseline | null> =>
    request('/api/analysis/baseline/get', {flowId}),

  setBaseline: (flowId: string = activeFlowId() ?? ''): Promise<FlowBaseline> =>
    request('/api/analysis/baseline/set', {flowId}),

  clearBaseline: (flowId: string = activeFlowId() ?? ''): Promise<void> =>
    request('/api/analysis/baseline/clear', {flowId}),

  baselineDrift: (flowId: string = activeFlowId() ?? ''): Promise<BaselineDrift> =>
    request('/api/analysis/baseline/drift', {flowId}),

  // --- Finding comments (persistent, team-shared; cloud mode only) ---

  listComments: (flowId: string, findingKey: string): Promise<FindingComment[]> =>
    request('/api/analysis/comments/list', {flowId, findingKey}),

  addComment: (flowId: string, findingKey: string, body: string): Promise<FindingComment> =>
    request('/api/analysis/comments/add', {flowId, findingKey, body}),

  deleteComment: (flowId: string, commentId: string): Promise<void> =>
    request('/api/analysis/comments/delete', {flowId, commentId}),
}
