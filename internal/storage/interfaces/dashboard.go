package interfaces

import (
	"context"
	"time"

	"pad-core/models"
)

// DashboardStore covers the persisted flow-health snapshots and the dashboard rollup queries.
type DashboardStore interface {
	SaveFlowAnalysis(ctx context.Context, fa *FlowAnalysis) error
	// LoadFlowHealth returns the flow's latest persisted analysis snapshot or (nil, nil) when never analyzed; caller-side authorization.
	LoadFlowHealth(ctx context.Context, flowID string) (*HealthSnapshot, error)
	// LoadFlowHealthBatch resolves many flows' snapshots in one query; map omits never-analyzed flows, never nil; caller-side authorization. Also the portfolio read: a snapshot carries its rule set's digest only.
	LoadFlowHealthBatch(ctx context.Context, flowIDs []string) (map[string]*HealthSnapshot, error)
	FlowDashboardData(ctx context.Context, ownerID string, days int) (*DashboardData, error)
	// FlowDashboardAdvanced returns trend, cost-by-provider, rule-frequency, activity, complexity-scatter, and security-posture data.
	FlowDashboardAdvanced(ctx context.Context, ownerID string, days int) (*DashboardAdvancedData, error)

	// Analytics reads (GET /api/dashboard/analytics); the service authorises the flow id set.
	LoadAnalysisSnapshots(ctx context.Context, flowIDs []string) ([]AnalysisSnapshotRow, error)
	LoadAnalysisHistory(ctx context.Context, w AnalyticsWindow) (*AnalysisHistoryRows, error)
	LoadTriageAnalytics(ctx context.Context, w AnalyticsWindow) (*TriageRows, error)
	// mode: "user" (the caller's calls), "org" (the scoped orgs'), "both".
	LoadUsageAnalytics(ctx context.Context, userID string, orgIDs []string, mode string, w AnalyticsWindow) (*UsageRows, error)
	LoadActivityAnalytics(ctx context.Context, userID string, w AnalyticsWindow, limit int) (*ActivityRows, error)
	LoadSecurityAnalytics(ctx context.Context, userID, email string, includeLocked bool) (*SecurityRows, error)
}

// HealthSnapshot is the persisted per-flow analysis summary surfaced on the single-flow GET; nil means never analyzed.
type HealthSnapshot struct {
	HealthScore int       `json:"healthScore"`
	Errors      int       `json:"errors"`
	Warnings    int       `json:"warnings"`
	Info        int       `json:"info"`
	AnalyzedAt  time.Time `json:"analyzedAt"`
	// RuleSetDigest is the digest of the rule set the analysis ran (ADR 0016), "" when unknown (a legacy snapshot);
	// RuleSetsByDigest resolves it. Not part of the flow GET.
	RuleSetDigest string `json:"-"`
}

// FlowAnalysis is the upserted summary of a flow's latest analysis (aggregates survive restarts); owner/org scoping is resolved by JOINing flows at read time — no denormalized ownership.
type FlowAnalysis struct {
	FlowID      string         `json:"flowId"`
	HealthScore int            `json:"healthScore"`
	Errors      int            `json:"errors"`
	Warnings    int            `json:"warnings"`
	Info        int            `json:"info"`
	ByCategory  map[string]int `json:"byCategory"`
	ByRule      map[string]int `json:"byRule"`
	AnalyzedAt  time.Time      `json:"analyzedAt"`
	// ByConfidence distributes findings across confidence tiers (high/medium/low) for the dashboard's "trust" donut.
	ByConfidence map[string]int `json:"byConfidence,omitempty"`
	// AutoFixableCount is how many findings carry a one-click deterministic fix (Finding.AutoFix != "").
	AutoFixableCount int `json:"autoFixableCount,omitempty"`
	// TotalFindings is the sum of Errors+Warnings+Info, denormalized for org-wide SUMs.
	TotalFindings int `json:"totalFindings,omitempty"`
	// RuleSet is the rule set the analysis ran (AnalysisReport.RuleSet); nil records none.
	RuleSet *models.RuleSet `json:"-"`
}

// RecentFlowHealth is one row of the dashboard's "recent flows" list: a flow with its latest health score (nil when never analyzed).
type RecentFlowHealth struct {
	FlowID      string
	Name        string
	HealthScore *int
	UpdatedAt   time.Time
}

// DailyTokens is one gap-filled day of AI token usage for the dashboard chart.
type DailyTokens struct {
	Date      string
	TokensIn  int
	TokensOut int
}

// DashboardData backs GET /api/dashboard/home in cloud mode; empty sections are conveyed by HealthCount (0 ⇒ nothing analyzed) and len(TokenUsage).
type DashboardData struct {
	TotalFlows    int
	TotalSubflows int
	HealthCount   int // number of analyzed flows contributing to AvgHealth
	AvgHealth     int
	Errors        int
	Warnings      int
	Info          int
	ByCategory    map[string]int
	Recent        []RecentFlowHealth
	TokenUsage    []DailyTokens
	// TotalFindings/AutoFixable/Confidence: org-wide findings sum, one-click-fixable count, certainty-tier distribution (dashboard KPIs).
	TotalFindings int
	AutoFixable   int
	Confidence    map[string]int
	// HealthBuckets is the org-wide health-score histogram (5 buckets of 20) exposing what AvgHealth hides.
	HealthBuckets []HealthBucket
}

// DailySeverityPoint is one day of the org-wide severity trend across every flow analyzed that day (stacked-area chart).
type DailySeverityPoint struct {
	Date     string
	Errors   int
	Warnings int
	Info     int
}

// HealthBucket is one 20-point-wide slice of the health-score histogram.
type HealthBucket struct {
	Label string // "0-20", "20-40", ...
	Lo    int
	Hi    int
	Count int
}

// DashboardAdvancedData extends DashboardData with trend, cost, rule, activity, complexity, and security sections.
type DashboardAdvancedData struct {
	HealthTrend []DailyHealthPoint
	CostByProv  []ProviderCost
	RuleFreq    []RuleFrequency
	Activity    []ActivityEntry
	Complexity  []FlowComplexityPoint
	Security    DashboardSecurity
	// SeverityTrend is the org-wide daily error/warning/info series for the stacked-area health-trend chart.
	SeverityTrend []DailySeverityPoint
	// Workflow is the team-triage funnel + resolution stats (MTTR, stale); cloud-only — local mode leaves Available false.
	Workflow WorkflowData
}

// WorkflowData holds the cloud-mode team-triage analytics: triage-state distribution, MTTR, and stale-finding count.
type WorkflowData struct {
	Funnel        map[string]int // status ("open"/"acknowledged"/"in_progress"/"resolved"/"suppressed") → count
	MttrHours     float64        // mean updated_at−created_at for resolved findings; 0 if none resolved
	StaleCount    int            // open/acknowledged untouched for > 14 days
	ResolvedCount int            // resolved findings contributing to MttrHours
}

// DailyHealthPoint is one day of the health-score trend chart.
type DailyHealthPoint struct {
	Date      string
	AvgHealth int
	FlowCount int
}

// ProviderCost aggregates AI spend by provider for the donut chart.
type ProviderCost struct {
	Provider  string
	Cost      float64
	TokensIn  int
	TokensOut int
}

// RuleFrequency is one rule's finding count across the owner's flows; severity tinting is a service-layer catalog concern.
type RuleFrequency struct {
	Rule  string
	Count int
}

// ActivityEntry is one row of the dashboard activity feed.
type ActivityEntry struct {
	Action    string
	FlowName  string
	CreatedAt time.Time
}

// FlowComplexityPoint is one flow's position on the complexity scatter.
type FlowComplexityPoint struct {
	FlowID       string
	FlowName     string
	BlockCount   int
	FindingCount int
	HealthScore  int
}

// DashboardSecurity summarizes security posture. Available is false when any count failed: each is a best-effort query whose failure leaves 0, which the UI would read as "no issues detected".
type DashboardSecurity struct {
	Available          bool
	FailedLogins24h    int
	LockedAccounts     int
	CredentialFindings int
}
