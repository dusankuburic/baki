// Welcome ("home") dashboard payload — GET /api/dashboard/home.
// Distinct from the in-session analytics aggregates (see DashboardStats in analysis.ts).

export interface DashboardHomeData {
  greeting: DashboardGreeting
  overview: DashboardOverview
  tokenUsage: DailyTokenUsage[]
  recentFlows: RecentFlowStub[]
  findings: DashboardFindingsAgg
  isCloud: boolean
  // Advanced sections (v2)
  healthTrend: DailyHealthPoint[]
  costByProvider: ProviderCost[]
  ruleFrequency: RuleFrequency[]
  activity: ActivityEntry[]
  complexity: FlowComplexityPoint[]
  security: DashboardSecurity
  // Analytics sections (v3) — developer-facing metrics. severityTrend is
  // cloud-only; confidenceDist/healthBuckets/fixability are populated in both
  // modes (local derives them from the session cache).
  severityTrend: DailySeverityPoint[]
  confidenceDist: Record<string, number>
  healthBuckets: HealthBucket[]
  fixability: Fixability
  // Team-triage workflow (cloud-only): status funnel, MTTR, stale findings.
  workflow: Workflow
}

export interface DashboardGreeting {
  userDisplayName: string
  activeOrgName?: string
}

export interface DashboardOverview {
  avgHealthScore: number
  healthAvailable: boolean
  totalFlows: number
  totalSubflows: number
}

export interface DailyTokenUsage {
  date: string
  tokensIn: number
  tokensOut: number
}

export interface RecentFlowStub {
  id: string
  name: string
  healthScore: number | null
  updatedAt: string
}

export interface DashboardFindingsAgg {
  available: boolean
  bySeverity: Record<string, number>
  byCategory: FindingCategory[]
}

export interface FindingCategory {
  category: string
  count: number
}

// ---- Advanced dashboard types (v2) ----

export interface DailyHealthPoint {
  date: string
  avgHealth: number
  flowCount: number
}

export interface ProviderCost {
  provider: string
  cost: number
  tokensIn: number
  tokensOut: number
}

export interface RuleFrequency {
  rule: string
  count: number
  /** Worst severity this rule fires at across the owner's flows ("error"/"warning"/"info"). Cloud-only. */
  topSeverity?: string
}

export interface ActivityEntry {
  action: string
  flowName?: string
  createdAt: string
}

export interface FlowComplexityPoint {
  flowId: string
  flowName: string
  blockCount: number
  findingCount: number
  healthScore: number
}

export interface DashboardSecurity {
  /** False when the posture could not be read: all-zero counts then mean "unknown", not "clean". */
  available: boolean
  failedLogins24h: number
  credentialFindings: number
  /** Accounts currently locked by the brute-force defense. Cloud-only. */
  lockedAccounts: number
  /** True when the caller may see instance-wide counts (lockedAccounts is zeroed otherwise). */
  admin: boolean
}

// ---- Analytics dashboard types (v3) ----

/** One day of the org-wide severity trend (error/warning/info summed across all flows). */
export interface DailySeverityPoint {
  date: string
  errors: number
  warnings: number
  info: number
}

/** One 20-point slice of the health-score histogram. */
export interface HealthBucket {
  label: string
  lo: number
  hi: number
  count: number
}

/** How much of the current finding load is cheaply fixable. */
export interface Fixability {
  /** Findings carrying a one-click deterministic fix. */
  available: number
  /** All findings. */
  total: number
  /** Rules in the catalog that ship a deterministic fixer. */
  autoFixableRules: number
  /** Total rules in the catalog. */
  totalRules: number
}

// ---- Analytics page payload — GET /api/dashboard/analytics ----

export type AnalyticsScopeKey = 'personal' | 'org' | 'all'

/** The filters the payload was computed for; flows the caller cannot see are dropped. */
export interface AnalyticsApplied {
  scope: AnalyticsScopeKey
  orgId?: string
  flows: string[]
  severity: string[]
  category: string[]
  rule: string[]
  compare: boolean
  tz: string
  /** True when a severity/category/rule filter is active: counts then come from by_rule × catalog. */
  derived: boolean
}

export interface AnalyticsRange {
  preset: string
  /** Inclusive local days, YYYY-MM-DD. */
  from: string
  to: string
  days: number
  prevFrom?: string
  prevTo?: string
}

/** One headline number: now (or as of the window's last day), the previous period, a ≤12-point sparkline. */
export interface StatValue {
  available: boolean
  value: number
  prev: number | null
  spark: number[]
}

export interface AnalyticsSummary {
  asOf: string
  flowsInScope: number
  unanalyzed: number
  avgHealth: StatValue
  openFindings: StatValue
  errors: StatValue
  warnings: StatValue
  info: StatValue
  autoFixable: StatValue
  analysesRun: StatValue
  flowsAnalyzed: StatValue
  resolved: StatValue
  aiSpend: StatValue
}

/** The fleet as of one day; avgHealth is null until any flow has a snapshot. */
export interface AnalyticsTrendPoint {
  date: string
  avgHealth: number | null
  flowCount: number
  runs: number
  findings: number
  errors: number
  warnings: number
  info: number
  autoFixable: number
}

export interface AnalyticsTrend {
  points: AnalyticsTrendPoint[]
  prevPoints: AnalyticsTrendPoint[]
}

export interface RuleCount {
  rule: string
  name: string
  category: string
  severity: string
  autoFixable: boolean
  inCatalog: boolean
  count: number
  flows: number
}

export interface CategoryCount {
  category: string
  count: number
  errors: number
  warnings: number
  info: number
}

export interface AnalyticsFindings {
  bySeverity: Record<string, number>
  byCategory: CategoryCount[]
  byRule: RuleCount[]
  byConfidence: Record<string, number>
  fixability: Fixability
}

export interface FlowPoint {
  flowId: string
  name: string
  ownerId: string
  blockCount: number
  findings: number
  errors: number
  warnings: number
  info: number
  health: number
  analyzedAt: string
}

export interface FlowMover {
  flowId: string
  name: string
  healthFrom: number | null
  healthTo: number | null
  delta: number
}

export interface AnalyticsFlows {
  healthBuckets: HealthBucket[]
  complexity: FlowPoint[]
  worst: FlowPoint[]
  movers: FlowMover[]
}

export interface DateCount {
  date: string
  count: number
}

export interface AnalyticsTriage {
  available: boolean
  funnel: Record<string, number>
  backlog: number
  mttrHours: number
  resolvedCount: number
  staleCount: number
  resolvedByDay: DateCount[]
}

export interface AIDailyPoint {
  date: string
  tokensIn: number
  tokensOut: number
  cost: number
  calls: number
}

export interface ModelUsage {
  provider: string
  model: string
  tokensIn: number
  tokensOut: number
  calls: number
  cost: number
}

/** AI usage for the caller and/or the scoped orgs; never tied to flows (flowScoped is always false). */
export interface AnalyticsAI {
  scoped: 'user' | 'org' | 'user+orgs'
  flowScoped: boolean
  daily: AIDailyPoint[]
  byProvider: ProviderCost[]
  byModel: ModelUsage[]
  totalCost: number
  prevCost: number | null
  totalTokens: number
}

export interface ActivityActor {
  id: string
  name: string
  isMe: boolean
}

export interface ActivityEvent {
  action: string
  actor: ActivityActor
  resourceType: string
  resourceId: string
  label: string
  createdAt: string
}

export interface ActionCount {
  action: string
  count: number
}

export interface AnalyticsActivity {
  events: ActivityEvent[]
  byDay: DateCount[]
  byAction: ActionCount[]
}

/** lockedAccounts is instance-wide and only present for instance admins. */
export interface AnalyticsSecurity {
  available: boolean
  failedLogins24h: number
  securityFindings: number
  credentialFindings: number
  lockedAccounts: number | null
}

export interface FacetCount {
  key: string
  label: string
  count: number
}

export interface FacetFlow {
  id: string
  name: string
  health: number | null
  findings: number
  analyzedAt: string | null
  orgId?: string
}

/** Chip counts (each dimension counted under the other filters) and the flow picker's list. */
export interface AnalyticsFacets {
  severity: FacetCount[]
  category: FacetCount[]
  rule: FacetCount[]
  flows: FacetFlow[]
  flowsTruncated: boolean
}

export interface DashboardAnalyticsData {
  applied: AnalyticsApplied
  range: AnalyticsRange
  summary: AnalyticsSummary
  trend: AnalyticsTrend
  findings: AnalyticsFindings
  flows: AnalyticsFlows
  triage: AnalyticsTriage
  ai: AnalyticsAI
  activity: AnalyticsActivity
  security: AnalyticsSecurity
  facets: AnalyticsFacets
  /** Section keys whose read failed; those sections are empty, not wrong. */
  partial: string[]
  generatedAt: string
}

/** Team-triage funnel + resolution health. Cloud-only (Available=false locally). */
export interface Workflow {
  /** True once any finding has been triaged; false ⇒ show a placeholder. */
  available: boolean
  /** status ("open"/"acknowledged"/"in_progress"/"resolved"/"suppressed") → count. */
  funnel: Record<string, number>
  /** Mean time to resolve, in hours; 0 if none resolved. */
  mttrHours: number
  /** Findings contributing to mttrHours. */
  resolvedCount: number
  /** Open/acknowledged findings untouched for > 14 days. */
  staleCount: number
}
