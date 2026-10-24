
export interface DashboardHomeData {
  greeting: DashboardGreeting
  overview: DashboardOverview
  tokenUsage: DailyTokenUsage[]
  recentFlows: RecentFlowStub[]
  findings: DashboardFindingsAgg
  isCloud: boolean
  healthTrend: DailyHealthPoint[]
  ruleFrequency: RuleFrequency[]
  activity: ActivityEntry[]
  complexity: FlowComplexityPoint[]
  security: DashboardSecurity
  severityTrend: DailySeverityPoint[]
  confidenceDist: Record<string, number>
  healthBuckets: HealthBucket[]
  fixability: Fixability
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

export interface DailyHealthPoint {
  date: string
  avgHealth: number
  flowCount: number
}

export interface ProviderUsage {
  provider: string
  tokensIn: number
  tokensOut: number
}

export interface RuleFrequency {
  rule: string
  count: number
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
  available: boolean
  failedLogins24h: number
  credentialFindings: number
  lockedAccounts: number
  admin: boolean
}

export interface DailySeverityPoint {
  date: string
  errors: number
  warnings: number
  info: number
}

export interface HealthBucket {
  label: string
  lo: number
  hi: number
  count: number
}

export interface Fixability {
  available: number
  total: number
  autoFixableRules: number
  totalRules: number
}

export type AnalyticsScopeKey = 'personal' | 'org' | 'all'

export interface AnalyticsApplied {
  scope: AnalyticsScopeKey
  orgId?: string
  flows: string[]
  severity: string[]
  category: string[]
  rule: string[]
  compare: boolean
  tz: string
  derived: boolean
}

export interface AnalyticsRange {
  preset: string
  from: string
  to: string
  days: number
  prevFrom?: string
  prevTo?: string
}

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
}

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
  calls: number
}

export interface ModelUsage {
  provider: string
  model: string
  servedModel?: string
  tokensIn: number
  tokensOut: number
  calls: number
}

export interface AnalyticsAI {
  scoped: 'user' | 'org' | 'user+orgs'
  flowScoped: boolean
  daily: AIDailyPoint[]
  byProvider: ProviderUsage[]
  byModel: ModelUsage[]
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
  partial: string[]
  generatedAt: string
}

export interface Workflow {
  available: boolean
  funnel: Record<string, number>
  mttrHours: number
  resolvedCount: number
  staleCount: number
}
