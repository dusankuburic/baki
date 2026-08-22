import {request, requestValidated} from './client'
import {useFlowStore} from '@/stores/flowStore'
import {getAnalysisReportSchema} from './schemas'

// Folder-wide batch analysis can run rules over many flows, well beyond the
// default request timeout.
const BATCH_ANALYSIS_TIMEOUT_MS = 300_000
import type {
  AnalysisReport,
  VariableHistory,
  GraphData,
  Rule,
  RuleConfig,
  FlowMetrics,
  DataFlowAnalysis,
  AnalysisSnapshot,
  BatchAnalysis,
  AnalysisDiff,
  DependencyAnalysis,
  DashboardStats,
  SubflowHash,
  DeduplicateResult,
  FlowComparison,
  Finding,
  FindingStatus,
  FlowBaseline,
  BaselineDrift,
  TriageStatus,
  FindingComment,
  Policy,
  PolicyResult,
} from '@/types'

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
  analyzeFlow: async (): Promise<AnalysisReport> =>
    requestValidated('/api/analysis/analyze', await getAnalysisReportSchema(), {
      body: {flowId: activeFlowId()},
    }),

  // analyzeFlowById re-analyzes an arbitrary flow the caller can view, without
  // requiring it to be the currently-open document. Used by the portfolio's bulk
  // re-analyze action (which operates on flows that aren't loaded in the editor).
  analyzeFlowById: async (flowId: string): Promise<AnalysisReport> =>
    requestValidated('/api/analysis/analyze', await getAnalysisReportSchema(), {body: {flowId}}),

  getVariableLineage: (varName: string): Promise<VariableHistory> =>
    request('/api/analysis/lineage', {body: {flowId: activeFlowId(), varName}}),

  getExecutionGraph: (): Promise<GraphData> => request('/api/analysis/graph', {body: {flowId: activeFlowId()}}),

  getMetrics: (): Promise<FlowMetrics> => request('/api/analysis/metrics', {body: {flowId: activeFlowId()}}),

  getDataFlow: (): Promise<DataFlowAnalysis> => request('/api/analysis/dataflow', {body: {flowId: activeFlowId()}}),

  getRules: (): Promise<Rule[]> => request('/api/analysis/rules', {method: 'GET'}),

  updateRuleConfig: (ruleId: string, config: RuleConfig): Promise<void> =>
    request('/api/analysis/rule/config', {body: {ruleId, config}}),

  setRuleEnabled: (ruleId: string, enabled: boolean): Promise<void> =>
    request('/api/analysis/rule/enabled', {body: {ruleId, enabled}}),

  getHistory: (): Promise<AnalysisSnapshot[]> => request('/api/analysis/history', {body: {flowId: activeFlowId()}}),

  batchAnalyze: (folderPath: string): Promise<BatchAnalysis> =>
    request('/api/analysis/batch', {body: {folderPath}, method: 'POST', timeoutMs: BATCH_ANALYSIS_TIMEOUT_MS}),

  getDiff: (): Promise<AnalysisDiff> => request('/api/analysis/diff', {body: {flowId: activeFlowId()}}),

  // HTML export lives in exportApi (single dialog-aware implementation for
  // PDF/Markdown/HTML
  // that omitted the save-dialog path).

  exportSARIF: (flowId: string = activeFlowId() ?? ''): Promise<unknown> =>
    request('/api/analysis/export/sarif', {body: {flowId}}),

  getDependencies: (): Promise<DependencyAnalysis> => request('/api/analysis/dependencies', {method: 'GET'}),

  getDashboard: (): Promise<DashboardStats> => request('/api/analysis/dashboard', {method: 'GET'}),

  getSubflowHashes: (): Promise<SubflowHash[]> =>
    request('/api/analysis/subflow-hashes', {body: {flowId: activeFlowId()}}),

  deduplicate: (): Promise<DeduplicateResult> => request('/api/analysis/deduplicate', {body: {flowId: activeFlowId()}}),

  getRelatedFindings: (blockId: string): Promise<Finding[]> =>
    request('/api/analysis/related', {body: {flowId: activeFlowId(), blockId}}),

  compareFlows: (flowAId: string, flowBId: string): Promise<FlowComparison> =>
    request('/api/analysis/compare', {body: {flowAId, flowBId}}),

  // --- Finding triage & baselines (persistent, team-shared; cloud mode only) ---

  listFindingStatuses: (flowId: string = activeFlowId() ?? ''): Promise<FindingStatus[]> =>
    request('/api/analysis/triage/list', {body: {flowId}}),

  setFindingStatus: (input: SetFindingStatusInput): Promise<FindingStatus> =>
    request('/api/analysis/triage/set', {body: {flowId: activeFlowId(), ...input}}),

  // Apply the same status to many findings of one flow in a single request.
  setFindingStatusBatch: (
    items: BatchFindingStatusItem[],
    flowId: string = activeFlowId() ?? '',
  ): Promise<{updated: number}> => request('/api/analysis/triage/set-batch', {body: {flowId, items}}),

  clearFindingStatus: (findingKey: string, flowId: string = activeFlowId() ?? ''): Promise<void> =>
    request('/api/analysis/triage/clear', {body: {flowId, findingKey}}),

  getBaseline: (flowId: string = activeFlowId() ?? ''): Promise<FlowBaseline | null> =>
    request('/api/analysis/baseline/get', {body: {flowId}}),

  setBaseline: (flowId: string = activeFlowId() ?? ''): Promise<FlowBaseline> =>
    request('/api/analysis/baseline/set', {body: {flowId}}),

  clearBaseline: (flowId: string = activeFlowId() ?? ''): Promise<void> =>
    request('/api/analysis/baseline/clear', {body: {flowId}}),

  baselineDrift: (flowId: string = activeFlowId() ?? ''): Promise<BaselineDrift> =>
    request('/api/analysis/baseline/drift', {body: {flowId}}),

  // --- Finding comments (persistent, team-shared; cloud mode only) ---

  listComments: (flowId: string, findingKey: string): Promise<FindingComment[]> =>
    request('/api/analysis/comments/list', {body: {flowId, findingKey}}),

  addComment: (flowId: string, findingKey: string, body: string): Promise<FindingComment> =>
    request('/api/analysis/comments/add', {body: {flowId, findingKey, body}}),

  deleteComment: (flowId: string, commentId: string): Promise<void> =>
    request('/api/analysis/comments/delete', {body: {flowId, commentId}}),

  // --- Policy Gates (persistent, org-scoped; cloud mode only) ---

  evaluatePolicy: (flowId: string, policy: Policy): Promise<PolicyResult> =>
    request('/api/analysis/policy/evaluate', {body: {flowId, policy}}),

  savePolicy: (policy: Policy): Promise<Policy> => request('/api/analysis/policy/save', {body: policy}),

  listPolicies: (orgId: string): Promise<Policy[]> => request('/api/analysis/policy/list', {body: {orgId}}),

  getPolicy: (orgId: string, id: string): Promise<Policy> => request('/api/analysis/policy/get', {body: {orgId, id}}),

  deletePolicy: (orgId: string, id: string): Promise<void> =>
    request('/api/analysis/policy/delete', {body: {orgId, id}}),

  evaluatePolicyById: (flowId: string, orgId: string, id: string): Promise<PolicyResult> =>
    request('/api/analysis/policy/evaluate-by-id', {body: {flowId, orgId, id}}),
}
