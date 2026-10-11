
import type {Tolerant} from './tolerant'

export type Severity = 'error' | 'warning' | 'info'

export type Confidence = 'high' | 'medium' | 'low'

export const RULE_STAGES = ['stable', 'preview'] as const
export type RuleStage = (typeof RULE_STAGES)[number]

export const RULE_KINDS = ['block', 'flow', 'library'] as const
export type RuleKind = (typeof RULE_KINDS)[number]

export interface Finding {
  id: string
  fingerprint?: string
  ruleId: string
  severity: Severity
  confidence?: Confidence
  title: string
  description: string
  blockId: string
  subflowId: string
  suggestion?: string
  autoFixHint?: string
  autoFix?: string
  category?: string
  metadata?: Record<string, unknown>
  stage?: Tolerant<RuleStage>
}

export const TRIAGE_STATUSES = ['open', 'acknowledged', 'in_progress', 'resolved', 'suppressed'] as const
export type TriageStatus = (typeof TRIAGE_STATUSES)[number]

export interface FindingStatus {
  flowId: string
  findingKey: string
  ruleId?: string
  status: Tolerant<TriageStatus>
  justification?: string
  assigneeId?: string
  updatedBy?: string
  updatedAt: string
}

export interface SuppressionEntry {
  line: number
  rule: string
  blockLabel: string
  blockType?: string
  subflow?: string
  stale: boolean
  reason?: string
}

export interface FlowBaseline {
  flowId: string
  keys: string[]
  createdBy?: string
  createdAt: string
  ruleSetDigest?: string
}

export interface BaselineDrift {
  flowId: string
  hasBaseline: boolean
  new: Finding[]
  newErrors: number
  newWarnings: number
  newInfo: number
  newRuleIds?: string[]
  newFromNewRules?: number
  preview?: number
}

export interface FindingComment {
  id: string
  flowId: string
  findingKey: string
  authorId: string
  authorName?: string
  body: string
  createdAt: string
}

export interface RuleProfile {
  ruleId: string
  ruleName: string
  durationMs: number
  findingCount: number
  blocksChecked: number
}

export interface AnalysisSnapshot {
  timestamp: string
  flowId: string
  hash: string
  errors: number
  warnings: number
  info: number
  healthScore: number
  durationMs: number
}

export interface AnalysisReport {
  flowId: string
  flowName?: string
  generatedAt: string
  findings: Finding[]
  stats: AnalysisStats
  durationMs: number
  metrics?: FlowMetrics
  ruleProfiles?: RuleProfile[]
}

export interface AnalysisStats {
  errors: number
  warnings: number
  info: number
  blocksAnalyzed: number
  rulesRun: number
  suppressed?: number
}

export interface SubflowMetrics {
  subflowId: string
  subflowName: string
  blockCount: number
  cyclomaticComplexity: number
  cognitiveComplexity: number
  maxNestingDepth: number
  variableCount: number
  fanIn: number
  fanOut: number
}

export interface FlowMetrics {
  subflows: SubflowMetrics[]
  totalBlocks: number
  totalVariables: number
  maxCyclomatic: number
  avgCyclomatic: number
  maxCognitive: number
  avgCognitive: number
  healthScore: number
  variableDensity: number
  subflowCount: number
  circularDependencies?: string[]
}

export interface BlockDataFlow {
  blockId: string
  subflowId: string
  reads: string[]
  writes: string[]
  upstreamBlocks: string[]
  downstreamBlocks: string[]
}

export interface TaintPath {
  sourceVar: string
  sourceBlock: string
  sinkBlock: string
  sinkType: string
  path: string[]
}

export interface DeadDataPath {
  variable: string
  setBlock: string
  readBlock: string
  reason: string
}

export interface DataFlowAnalysis {
  blocks: Record<string, BlockDataFlow>
  taintPaths: TaintPath[]
  deadData: DeadDataPath[]
}

export interface BatchResult {
  flowId: string
  flowName: string
  report: AnalysisReport
  error?: string
}

export interface BatchAnalysis {
  results: BatchResult[]
  totalFlows: number
  totalFindings: number
  totalErrors: number
  totalWarnings: number
  totalInfo: number
  avgHealthScore: number
  durationMs: number
}

export interface AnalysisDiff {
  flowId: string
  added: Finding[]
  removed: Finding[]
  persisted: Finding[]
  addedCount: number
  removedCount: number
  persistedCount: number
  hasPrevious: boolean
}

export interface RuleDependency {
  fromRuleId: string
  toRuleId: string
  reason: string
}

export interface DependencyAnalysis {
  dependencies: RuleDependency[]
  cycles: string[][]
  topoOrder: string[]
}

export interface DashboardStats {
  totalFlowsAnalyzed: number
  totalFindings: number
  findingsBySeverity: Record<string, number>
  findingsByCategory: Record<string, number>
  findingsByRule: Record<string, number>
  avgHealthScore: number
  topProblemFlows: ProblemFlow[]
}

export interface ProblemFlow {
  flowId: string
  flowName: string
  findingCount: number
  healthScore: number
}

export interface FindingGroup {
  blockId: string
  findings: Finding[]
  primary: Finding
  duplicateCount: number
}

export interface DeduplicateResult {
  deduplicated: Finding[]
  groups: FindingGroup[]
  originalCount: number
  dedupedCount: number
}

export interface Rule {
  id: string
  name: string
  description: string
  defaultSeverity: Severity
  category: string
  enabled: boolean
  confidence?: Confidence
  autoFix?: string
  options?: Record<string, number>
  stage?: Tolerant<RuleStage>
  introducedIn?: number
  kind?: Tolerant<RuleKind>
}

export interface RuleConfig {
  enabled: boolean
  severity: Severity
  options?: Record<string, unknown>
}

export interface PolicyRule {
  ruleId: string
  enabled: boolean
  severity?: Severity
}

export interface Policy {
  id: string
  orgId?: string
  name: string
  description?: string
  rules: PolicyRule[]
  gateSeverity?: Severity
  createdAt?: string
  updatedAt?: string
}

export interface PolicyResult {
  policyId: string
  policyName: string
  passed: boolean
  violations: Finding[]
  errors: number
  warnings: number
  info: number
  evaluated: number
  preview?: number
}
