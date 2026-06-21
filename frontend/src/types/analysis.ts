// Analyzer outputs — findings, metrics, batch results, diffs, rule definitions.

export type Severity = 'error' | 'warning' | 'info';

export interface Finding {
  id: string;
  // Stable, content-derived identity (ruleId:blockId) from the backend. Unlike
  // `id` (a per-run index like "F-001" that shifts as findings come and go),
  // `fingerprint` survives re-analysis — use it to key triage state, suppressions,
  // and baselines. Optional for backward compatibility with older payloads.
  fingerprint?: string;
  ruleId: string;
  severity: Severity;
  title: string;
  description: string;
  blockId: string;
  subflowId: string;
  suggestion?: string;
  autoFixHint?: string;
  category?: string;
  metadata?: Record<string, unknown>;
}

// Triage lifecycle for a finding. Mirrors the backend's validTriageStatuses.
export type TriageStatus =
  | 'open'
  | 'acknowledged'
  | 'in_progress'
  | 'resolved'
  | 'suppressed';

// Persisted, team-shared triage state for one finding, keyed by its stable
// fingerprint (findingKey == Finding.fingerprint == ruleId:blockId).
export interface FindingStatus {
  flowId: string;
  findingKey: string;
  ruleId?: string;
  status: TriageStatus;
  justification?: string;
  assigneeId?: string;
  updatedBy?: string;
  updatedAt: string;
}

// The accepted set of finding keys for a flow. Findings whose fingerprint is not
// in `keys` are "new since baseline".
export interface FlowBaseline {
  flowId: string;
  keys: string[];
  createdBy?: string;
  createdAt: string;
}

export interface RuleProfile {
  ruleId: string;
  ruleName: string;
  durationMs: number;
  findingCount: number;
  blocksChecked: number;
}

export interface AnalysisSnapshot {
  timestamp: string;
  flowId: string;
  hash: string;
  errors: number;
  warnings: number;
  info: number;
  healthScore: number;
  durationMs: number;
}

export interface AnalysisReport {
  flowId: string;
  flowName?: string;
  generatedAt: string;
  findings: Finding[];
  stats: AnalysisStats;
  durationMs: number;
  metrics?: FlowMetrics;
  ruleProfiles?: RuleProfile[];
}

export interface AnalysisStats {
  errors: number;
  warnings: number;
  info: number;
  blocksAnalyzed: number;
  rulesRun: number;
}

export interface SubflowMetrics {
  subflowId: string;
  subflowName: string;
  blockCount: number;
  cyclomaticComplexity: number;
  cognitiveComplexity: number;
  maxNestingDepth: number;
  variableCount: number;
  fanIn: number;
  fanOut: number;
}

export interface FlowMetrics {
  subflows: SubflowMetrics[];
  totalBlocks: number;
  totalVariables: number;
  maxCyclomatic: number;
  avgCyclomatic: number;
  maxCognitive: number;
  avgCognitive: number;
  healthScore: number;
  variableDensity: number;
  subflowCount: number;
  circularDependencies?: string[];
}

export interface BlockDataFlow {
  blockId: string;
  subflowId: string;
  reads: string[];
  writes: string[];
  upstreamBlocks: string[];
  downstreamBlocks: string[];
}

export interface TaintPath {
  sourceVar: string;
  sourceBlock: string;
  sinkBlock: string;
  sinkType: string;
  path: string[];
}

export interface DeadDataPath {
  variable: string;
  setBlock: string;
  readBlock: string;
  reason: string;
}

export interface DataFlowAnalysis {
  blocks: Record<string, BlockDataFlow>;
  taintPaths: TaintPath[];
  deadData: DeadDataPath[];
}

export interface BatchResult {
  flowId: string;
  flowName: string;
  report: AnalysisReport;
  error?: string;
}

export interface BatchAnalysis {
  results: BatchResult[];
  totalFlows: number;
  totalFindings: number;
  totalErrors: number;
  totalWarnings: number;
  totalInfo: number;
  avgHealthScore: number;
  durationMs: number;
}

export interface AnalysisDiff {
  flowId: string;
  added: Finding[];
  removed: Finding[];
  persisted: Finding[];
  addedCount: number;
  removedCount: number;
  persistedCount: number;
  /** False when there is no earlier run to compare against. */
  hasPrevious: boolean;
}

export interface RuleDependency {
  fromRuleId: string;
  toRuleId: string;
  reason: string;
}

export interface DependencyAnalysis {
  dependencies: RuleDependency[];
  cycles: string[][];
  topoOrder: string[];
}

export interface SubflowHash {
  subflowId: string;
  hash: string;
}

// In-session analytics aggregates (powers AnalyticsDashboard.tsx).
// Distinct from the welcome dashboard's payload, which lives in dashboard.ts.
export interface DashboardStats {
  totalFlowsAnalyzed: number;
  totalFindings: number;
  findingsBySeverity: Record<string, number>;
  findingsByCategory: Record<string, number>;
  findingsByRule: Record<string, number>;
  avgHealthScore: number;
  topProblemFlows: ProblemFlow[];
}

export interface ProblemFlow {
  flowId: string;
  flowName: string;
  findingCount: number;
  healthScore: number;
}

export interface FindingGroup {
  blockId: string;
  findings: Finding[];
  primary: Finding;
  duplicateCount: number;
}

export interface DeduplicateResult {
  deduplicated: Finding[];
  groups: FindingGroup[];
  originalCount: number;
  dedupedCount: number;
}

// Rule definition + per-flow override config. RuleConfig is consumed by
// AppSettings.analysis (see settings.ts).
export interface Rule {
  id: string
  name: string
  description: string
  defaultSeverity: Severity
  category: string
  enabled: boolean
}

export interface RuleConfig {
  enabled: boolean;
  severity: Severity;
  options?: Record<string, unknown>;
}
