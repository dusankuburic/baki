// Welcome ("home") dashboard payload — GET /api/dashboard/home.
// Distinct from the in-session analytics aggregates (see DashboardStats in analysis.ts).

export interface DashboardHomeData {
  greeting: DashboardGreeting;
  overview: DashboardOverview;
  tokenUsage: DailyTokenUsage[];
  recentFlows: RecentFlowStub[];
  findings: DashboardFindingsAgg;
  isCloud: boolean;
  // Advanced sections (v2)
  healthTrend: DailyHealthPoint[];
  costByProvider: ProviderCost[];
  ruleFrequency: RuleFrequency[];
  activity: ActivityEntry[];
  complexity: FlowComplexityPoint[];
  security: DashboardSecurity;
  // Analytics sections (v3) — developer-facing metrics. severityTrend is
  // cloud-only; confidenceDist/healthBuckets/fixability are populated in both
  // modes (local derives them from the session cache).
  severityTrend: DailySeverityPoint[];
  confidenceDist: Record<string, number>;
  healthBuckets: HealthBucket[];
  fixability: Fixability;
  // Team-triage workflow (cloud-only): status funnel, MTTR, stale findings.
  workflow: Workflow;
}

export interface DashboardGreeting {
  userDisplayName: string;
  activeOrgName?: string;
}

export interface DashboardOverview {
  avgHealthScore: number;
  healthAvailable: boolean;
  totalFlows: number;
  totalSubflows: number;
}

export interface DailyTokenUsage {
  date: string;
  tokensIn: number;
  tokensOut: number;
}

export interface RecentFlowStub {
  id: string;
  name: string;
  healthScore: number | null;
  updatedAt: string;
}

export interface DashboardFindingsAgg {
  available: boolean;
  bySeverity: Record<string, number>;
  byCategory: FindingCategory[];
}

export interface FindingCategory {
  category: string;
  count: number;
}

// ---- Advanced dashboard types (v2) ----

export interface DailyHealthPoint {
  date: string;
  avgHealth: number;
  flowCount: number;
}

export interface ProviderCost {
  provider: string;
  cost: number;
  tokensIn: number;
  tokensOut: number;
}

export interface RuleFrequency {
  rule: string;
  count: number;
  /** Worst severity this rule fires at across the owner's flows ("error"/"warning"/"info"). Cloud-only. */
  topSeverity?: string;
}

export interface ActivityEntry {
  action: string;
  flowName?: string;
  createdAt: string;
}

export interface FlowComplexityPoint {
  flowId: string;
  flowName: string;
  blockCount: number;
  findingCount: number;
  healthScore: number;
}

export interface DashboardSecurity {
  failedLogins24h: number;
  credentialFindings: number;
  /** Accounts currently locked by the brute-force defense. Cloud-only. */
  lockedAccounts: number;
}

// ---- Analytics dashboard types (v3) ----

/** One day of the org-wide severity trend (error/warning/info summed across all flows). */
export interface DailySeverityPoint {
  date: string;
  errors: number;
  warnings: number;
  info: number;
}

/** One 20-point slice of the health-score histogram. */
export interface HealthBucket {
  label: string;
  lo: number;
  hi: number;
  count: number;
}

/** How much of the current finding load is cheaply fixable. */
export interface Fixability {
  /** Findings carrying a one-click deterministic fix. */
  available: number;
  /** All findings. */
  total: number;
  /** Rules in the catalog that ship a deterministic fixer. */
  autoFixableRules: number;
  /** Total rules in the catalog. */
  totalRules: number;
}

/** Team-triage funnel + resolution health. Cloud-only (Available=false locally). */
export interface Workflow {
  /** True once any finding has been triaged; false ⇒ show a placeholder. */
  available: boolean;
  /** status ("open"/"acknowledged"/"in_progress"/"resolved"/"suppressed") → count. */
  funnel: Record<string, number>;
  /** Mean time to resolve, in hours; 0 if none resolved. */
  mttrHours: number;
  /** Findings contributing to mttrHours. */
  resolvedCount: number;
  /** Open/acknowledged findings untouched for > 14 days. */
  staleCount: number;
}
