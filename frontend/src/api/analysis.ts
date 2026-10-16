import {request, requestText, requestValidated} from './client'
import {useFlowStore} from '@/stores/flowStore'
import {getAnalysisReportSchema, getPoliciesSchema, getPolicySchema} from './schemas'

const BATCH_ANALYSIS_TIMEOUT_MS = 300_000
import type {
  AnalysisReport,
  VariableHistory,
  GraphData,
  Rule,
  RuleConfig,
  DataFlowAnalysis,
  AnalysisSnapshot,
  BatchAnalysis,
  AnalysisDiff,
  DependencyAnalysis,
  DashboardStats,
  DeduplicateResult,
  FlowComparison,
  Finding,
  FindingStatus,
  FlowBaseline,
  BaselineDrift,
  SuppressionEntry,
  TriageStatus,
  FindingComment,
  Policy,
  PolicyResult,
} from '@/types'

export interface SetFindingStatusInput {
  findingKey: string
  status: TriageStatus
  ruleId?: string
  justification?: string
  assigneeId?: string
  flowId?: string
}

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

  analyzeFlowById: async (flowId: string): Promise<AnalysisReport> =>
    requestValidated('/api/analysis/analyze', await getAnalysisReportSchema(), {body: {flowId}}),

  getVariableLineage: (varName: string): Promise<VariableHistory> =>
    request('/api/analysis/lineage', {body: {flowId: activeFlowId(), varName}}),

  getExecutionGraph: (): Promise<GraphData> => request('/api/analysis/graph', {body: {flowId: activeFlowId()}}),

  getDataFlow: (): Promise<DataFlowAnalysis> => request('/api/analysis/dataflow', {body: {flowId: activeFlowId()}}),

  getRules: (orgId?: string): Promise<Rule[]> =>
    request(`/api/analysis/rules${orgId ? `?orgId=${encodeURIComponent(orgId)}` : ''}`, {method: 'GET'}),

  updateRuleConfig: (ruleId: string, config: RuleConfig): Promise<void> =>
    request('/api/analysis/rule/config', {body: {ruleId, config}}),

  validateCustomRules: (rules: unknown[]): Promise<CustomRuleValidation> =>
    request('/api/analysis/custom-rules/validate', {body: {rules}}),

  testCustomRule: (rule: unknown, flowId: string): Promise<CustomRuleTestResult> =>
    request('/api/analysis/custom-rules/test', {body: {rule, flowId}}),

  setRuleEnabled: (ruleId: string, enabled: boolean): Promise<void> =>
    request('/api/analysis/rule/enabled', {body: {ruleId, enabled}}),

  getHistory: (): Promise<AnalysisSnapshot[]> => request('/api/analysis/history', {body: {flowId: activeFlowId()}}),

  batchAnalyze: (folderPath: string): Promise<BatchAnalysis> =>
    request('/api/analysis/batch', {body: {folderPath}, method: 'POST', timeoutMs: BATCH_ANALYSIS_TIMEOUT_MS}),

  getDiff: (): Promise<AnalysisDiff> => request('/api/analysis/diff', {body: {flowId: activeFlowId()}}),

  exportSARIF: (flowId: string = activeFlowId() ?? ''): Promise<string> =>
    requestText('/api/analysis/export/sarif', {body: {flowId}}),

  exportJUnit: (flowId: string = activeFlowId() ?? ''): Promise<string> =>
    requestText('/api/analysis/export/junit', {body: {flowId}}),

  listSuppressions: (flowId: string = activeFlowId() ?? ''): Promise<SuppressionEntry[]> =>
    request('/api/analysis/suppressions', {body: {flowId}}),

  getDependencies: (): Promise<DependencyAnalysis> => request('/api/analysis/dependencies', {method: 'GET'}),

  getDashboard: (): Promise<DashboardStats> => request('/api/analysis/dashboard', {method: 'GET'}),

  deduplicate: (): Promise<DeduplicateResult> => request('/api/analysis/deduplicate', {body: {flowId: activeFlowId()}}),

  getRelatedFindings: (blockId: string): Promise<Finding[]> =>
    request('/api/analysis/related', {body: {flowId: activeFlowId(), blockId}}),

  compareFlows: (flowAId: string, flowBId: string): Promise<FlowComparison> =>
    request('/api/analysis/compare', {body: {flowAId, flowBId}}),

  listFindingStatuses: (flowId: string = activeFlowId() ?? ''): Promise<FindingStatus[]> =>
    request('/api/analysis/triage/list', {body: {flowId}}),

  setFindingStatus: (input: SetFindingStatusInput): Promise<FindingStatus> =>
    request('/api/analysis/triage/set', {body: {flowId: activeFlowId(), ...input}}),

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

  listComments: (flowId: string, findingKey: string): Promise<FindingComment[]> =>
    request('/api/analysis/comments/list', {body: {flowId, findingKey}}),

  addComment: (flowId: string, findingKey: string, body: string): Promise<FindingComment> =>
    request('/api/analysis/comments/add', {body: {flowId, findingKey, body}}),

  deleteComment: (flowId: string, commentId: string): Promise<void> =>
    request('/api/analysis/comments/delete', {body: {flowId, commentId}}),

  savePolicy: async (policy: Policy): Promise<Policy> =>
    requestValidated('/api/analysis/policy/save', await getPolicySchema(), {body: policy}),

  listPolicies: async (orgId: string): Promise<Policy[]> =>
    requestValidated('/api/analysis/policy/list', await getPoliciesSchema(), {body: {orgId}}),

  deletePolicy: (orgId: string, id: string): Promise<void> =>
    request('/api/analysis/policy/delete', {body: {orgId, id}}),

  evaluatePolicyById: (flowId: string, orgId: string, id: string): Promise<PolicyResult> =>
    request('/api/analysis/policy/evaluate-by-id', {body: {flowId, orgId, id}}),
}

export interface CustomRuleProblem {
  field?: string
  reason: string
  message: string
  warning?: boolean
}

export interface CustomRuleValidationEntry {
  index: number
  id: string
  valid: boolean
  error?: string
  field?: string
  reason?: string
  problems?: CustomRuleProblem[]
  loaded: boolean
}

export interface CustomRuleValidation {
  entries: CustomRuleValidationEntry[]
  valid: number
  invalid: number
}

export interface CustomRuleTestResult {
  matches: number
  flowName: string
  findings: {ruleId: string; title: string; blockId: string}[]
  /** What the strict check says of the rule it ran (warnings, and a built-in rule's id); absent before the server
   * ran rules under a built-in id. */
  problems?: CustomRuleProblem[]
}
