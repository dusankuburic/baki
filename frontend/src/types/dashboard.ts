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
}
