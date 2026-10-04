package models

import "time"

// DashboardHomeData is the payload for GET /api/dashboard/home — assembled
// per-section so one source's failure still returns the rest; sections
// without data carry explicit availability flags so the UI renders honest
// empty states, not zeros.
type DashboardHomeData struct {
	Greeting    DashboardGreeting    `json:"greeting"`
	Overview    DashboardOverview    `json:"overview"`
	TokenUsage  []DailyTokenUsage    `json:"tokenUsage"`
	RecentFlows []RecentFlowStub     `json:"recentFlows"`
	Findings    DashboardFindingsAgg `json:"findings"`
	// Mode hints for the UI (e.g. to hide cards that have no local source)
	IsCloud bool `json:"isCloud"`
	// Advanced sections (v2). Populated in cloud mode; empty in local mode.
	HealthTrend []DailyHealthPoint    `json:"healthTrend"`
	CostByProv  []ProviderCostStub    `json:"costByProvider"`
	RuleFreq    []RuleFrequencyStub   `json:"ruleFrequency"`
	Activity    []ActivityStub        `json:"activity"`
	Complexity  []FlowComplexityStub  `json:"complexity"`
	Security    DashboardSecurityStub `json:"security"`
	// Analytics sections (v3): confidence, health histogram and fixability
	// answer developer-facing questions the v2 cards didn't; SeverityTrend is
	// cloud-only (needs history).
	SeverityTrend  []DailySeverityPoint `json:"severityTrend"`
	ConfidenceDist map[string]int       `json:"confidenceDist"`
	HealthBuckets  []HealthBucketStub   `json:"healthBuckets"`
	Fixability     FixabilityStub       `json:"fixability"`
	// Workflow is the team-triage funnel + resolution health (MTTR, stale).
	// Cloud-only: local mode keeps Available false and renders a placeholder.
	Workflow WorkflowStub `json:"workflow"`
}

type DashboardGreeting struct {
	UserDisplayName string `json:"userDisplayName"`
	ActiveOrgName   string `json:"activeOrgName,omitempty"`
}

type DashboardOverview struct {
	AvgHealthScore  int  `json:"avgHealthScore"`
	HealthAvailable bool `json:"healthAvailable"` // false ⇒ no analysis data yet; UI shows a placeholder, not a 0% gauge
	TotalFlows      int  `json:"totalFlows"`
	TotalSubflows   int  `json:"totalSubflows"`
}

type DailyTokenUsage struct {
	Date      string `json:"date"`
	TokensIn  int    `json:"tokensIn"`
	TokensOut int    `json:"tokensOut"`
}

type RecentFlowStub struct {
	ID          string    `json:"id"`
	Name        string    `json:"name"`
	HealthScore *int      `json:"healthScore"` // nullable: no analysis snapshot for this flow yet
	UpdatedAt   time.Time `json:"updatedAt"`
}

type DashboardFindingsAgg struct {
	Available  bool              `json:"available"`
	BySeverity map[string]int    `json:"bySeverity"`
	ByCategory []FindingCategory `json:"byCategory"`
}

type FindingCategory struct {
	Category string `json:"category"`
	Count    int    `json:"count"`
}

type DailyHealthPoint struct {
	Date      string `json:"date"`
	AvgHealth int    `json:"avgHealth"`
	FlowCount int    `json:"flowCount"`
}

type ProviderCostStub struct {
	Provider  string  `json:"provider"`
	Cost      float64 `json:"cost"`
	TokensIn  int     `json:"tokensIn"`
	TokensOut int     `json:"tokensOut"`
}

type RuleFrequencyStub struct {
	Rule  string `json:"rule"`
	Count int    `json:"count"`
	// TopSeverity is the worst severity this rule fires at across the owner's
	// flows, so the bar chart can tint each bar by severity.
	TopSeverity string `json:"topSeverity,omitempty"`
}

type ActivityStub struct {
	Action    string    `json:"action"`
	FlowName  string    `json:"flowName,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
}

type FlowComplexityStub struct {
	FlowID       string `json:"flowId"`
	FlowName     string `json:"flowName"`
	BlockCount   int    `json:"blockCount"`
	FindingCount int    `json:"findingCount"`
	HealthScore  int    `json:"healthScore"`
}

type DashboardSecurityStub struct {
	// Available is false when the posture could not be read (query failed, or
	// no persistent source in local mode): all-zero counts are then "unknown",
	// not "no issues".
	Available bool `json:"available"`
	// Admin is set per request: LockedAccounts is an instance-wide count and
	// is zeroed for everyone else.
	Admin              bool `json:"admin"`
	FailedLogins24h    int  `json:"failedLogins24h"`
	CredentialFindings int  `json:"credentialFindings"`
	// LockedAccounts is how many accounts the brute-force defense currently
	// holds locked, so admins can spot a sweep without grepping the audit log.
	LockedAccounts int `json:"lockedAccounts"`
}

// DailySeverityPoint is one day of the org-wide severity trend (error/warning/
// info summed across every flow analyzed that day); drives the stacked-area
// fleet-health chart. Cloud-only.
type DailySeverityPoint struct {
	Date     string `json:"date"`
	Errors   int    `json:"errors"`
	Warnings int    `json:"warnings"`
	Info     int    `json:"info"`
}

// HealthBucketStub is one 20-point slice of the health-score histogram,
// rendering the distribution the single average hides (a 70 built from a
// bimodal 40/100 split).
type HealthBucketStub struct {
	Label string `json:"label"` // "0-20", "20-40", ...
	Lo    int    `json:"lo"`
	Hi    int    `json:"hi"`
	Count int    `json:"count"`
}

// FixabilityStub summarizes how much of the current finding load is cheaply
// fixable: Available counts findings carrying a one-click deterministic fix,
// Total all findings; AutoFixableRules/TotalRules come from the catalog.
type FixabilityStub struct {
	Available        int `json:"available"`
	Total            int `json:"total"`
	AutoFixableRules int `json:"autoFixableRules"`
	TotalRules       int `json:"totalRules"`
}

// WorkflowStub carries the team-triage funnel and resolution health.
// Available is false in local mode (no persistent triage) so the UI shows a
// placeholder instead of an all-zero funnel.
type WorkflowStub struct {
	Available     bool           `json:"available"`
	Funnel        map[string]int `json:"funnel"`        // status → count (open/acknowledged/in_progress/resolved/suppressed)
	MttrHours     float64        `json:"mttrHours"`     // mean time to resolve; 0 if none resolved
	ResolvedCount int            `json:"resolvedCount"` // findings contributing to MttrHours
	StaleCount    int            `json:"staleCount"`    // open/acknowledged untouched > 14d
}
