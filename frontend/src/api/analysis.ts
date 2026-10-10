import {request, requestText, requestValidated} from './client'
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

  getDataFlow: (): Promise<DataFlowAnalysis> => request('/api/analysis/dataflow', {body: {flowId: activeFlowId()}}),

  // orgId scopes the catalog to that organisation's custom rules and
  // enable/severity/threshold overrides — the rules that actually run on its
  // flows. Without it the deployment catalog is returned.
  getRules: (orgId?: string): Promise<Rule[]> =>
    request(`/api/analysis/rules${orgId ? `?orgId=${encodeURIComponent(orgId)}` : ''}`, {method: 'GET'}),

  updateRuleConfig: (ruleId: string, config: RuleConfig): Promise<void> =>
    request('/api/analysis/rule/config', {body: {ruleId, config}}),

  /**
   * Validates custom-rule configs against the SERVER's analyzer — the same
   * construction path the save endpoint and the analysis engine use, so the
   * editor's verdict cannot disagree with either.
   *
   * Returns one entry per input rule, in order.
   */
  validateCustomRules: (rules: unknown[]): Promise<CustomRuleValidation> =>
    request('/api/analysis/custom-rules/validate', {body: {rules}}),

  /**
   * Runs an UNSAVED rule against one of the caller's flows and reports what it
   * would match.
   *
   * Distinct from validateCustomRules on purpose: that answers "does this
   * compile", this answers "does it do anything". A regex that compiles and
   * matches nothing is a policy the org believes is enforced and is not.
   */
  testCustomRule: (rule: unknown, flowId: string): Promise<CustomRuleTestResult> =>
    request('/api/analysis/custom-rules/test', {body: {rule, flowId}}),

  setRuleEnabled: (ruleId: string, enabled: boolean): Promise<void> =>
    request('/api/analysis/rule/enabled', {body: {ruleId, enabled}}),

  getHistory: (): Promise<AnalysisSnapshot[]> => request('/api/analysis/history', {body: {flowId: activeFlowId()}}),

  batchAnalyze: (folderPath: string): Promise<BatchAnalysis> =>
    request('/api/analysis/batch', {body: {folderPath}, method: 'POST', timeoutMs: BATCH_ANALYSIS_TIMEOUT_MS}),

  getDiff: (): Promise<AnalysisDiff> => request('/api/analysis/diff', {body: {flowId: activeFlowId()}}),

  // HTML export lives in exportApi (single dialog-aware implementation for
  // PDF/Markdown/HTML
  // that omitted the save-dialog path).

  // Server-rendered SARIF 2.1.0 and JUnit XML, as text: the saved file is the
  // server's bytes (request() would JSON-parse them, which fails for XML). CSV
  // is built client-side from the visible findings, so there is no server CSV
  // wrapper.
  exportSARIF: (flowId: string = activeFlowId() ?? ''): Promise<string> =>
    requestText('/api/analysis/export/sarif', {body: {flowId}}),

  exportJUnit: (flowId: string = activeFlowId() ?? ''): Promise<string> =>
    requestText('/api/analysis/export/junit', {body: {flowId}}),

  // Inline pad-ignore directives in the flow, with stale ones flagged. The
  // audit ran only in the CLI (`bakicli suppressions`) before this route.
  listSuppressions: (flowId: string = activeFlowId() ?? ''): Promise<SuppressionEntry[]> =>
    request('/api/analysis/suppressions', {body: {flowId}}),

  getDependencies: (): Promise<DependencyAnalysis> => request('/api/analysis/dependencies', {method: 'GET'}),

  getDashboard: (): Promise<DashboardStats> => request('/api/analysis/dashboard', {method: 'GET'}),

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

  savePolicy: (policy: Policy): Promise<Policy> => request('/api/analysis/policy/save', {body: policy}),

  listPolicies: (orgId: string): Promise<Policy[]> => request('/api/analysis/policy/list', {body: {orgId}}),

  deletePolicy: (orgId: string, id: string): Promise<void> =>
    request('/api/analysis/policy/delete', {body: {orgId, id}}),

  evaluatePolicyById: (flowId: string, orgId: string, id: string): Promise<PolicyResult> =>
    request('/api/analysis/policy/evaluate-by-id', {body: {flowId, orgId, id}}),
}

/** One problem of a validated custom rule: an error, or a warning the rule still runs with. */
export interface CustomRuleProblem {
  /** The rule's JSON field (`severity`, `nameMatch`, an unknown key); absent for the rule as a whole. */
  field?: string
  /** The analyzer's reason code, e.g. UNKNOWN_FIELD, RESERVED_ID, INVALID_PATTERN, NO_MATCHER. */
  reason: string
  message: string
  warning?: boolean
}

/** Per-entry result from POST /api/analysis/custom-rules/validate (checked strictly, as a save is). */
export interface CustomRuleValidationEntry {
  index: number
  id: string
  valid: boolean
  /** The first error, with its field and reason. */
  error?: string
  field?: string
  reason?: string
  /** Every error and warning, in check order. */
  problems?: CustomRuleProblem[]
  loaded: boolean
}

export interface CustomRuleValidation {
  entries: CustomRuleValidationEntry[]
  valid: number
  invalid: number
}

/** Result of POST /api/analysis/custom-rules/test. */
export interface CustomRuleTestResult {
  matches: number
  flowName: string
  findings: {ruleId: string; title: string; blockId: string}[]
}
