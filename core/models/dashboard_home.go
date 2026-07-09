package models

import "time"

// DashboardHomeData is the payload for GET /api/dashboard/home — the welcome
// "mission control" landing surface. It is assembled per-section so a failure in
// one source (e.g. token usage) still returns the rest; sections that have no
// data carry explicit availability flags so the UI can render honest empty
// states instead of misleading zeros.
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
	// Analytics sections (v3). These answer developer-facing questions the v2
	// cards didn't: "is the fleet getting healthier" (severity trend), "how much
	// can I trust these results" (confidence), "where does health cluster"
	// (histogram), and "how much is auto-fixable" (fixability). SeverityTrend is
	// cloud-only (needs history); the rest are populated in both modes.
	SeverityTrend  []DailySeverityPoint `json:"severityTrend"`
	ConfidenceDist map[string]int       `json:"confidenceDist"`
	HealthBuckets  []HealthBucketStub   `json:"healthBuckets"`
	Fixability     FixabilityStub       `json:"fixability"`
	// Workflow is the team-triage funnel + resolution health (MTTR, stale).
	// Cloud-only: local mode has no persistent triage, so Available stays false
	// and the UI renders a placeholder rather than an empty funnel.
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

// ---- Advanced dashboard types (v2) ----

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
	// flows ("error"/"warning"/"info"), so the bar chart can tint each bar by
	// severity instead of a flat brand color.
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
	FailedLogins24h    int `json:"failedLogins24h"`
	CredentialFindings int `json:"credentialFindings"`
	// LockedAccounts is how many user accounts are currently locked by the
	// brute-force defense, surfaced so admins can spot a sweep without grepping
	// the audit log.
	LockedAccounts int `json:"lockedAccounts"`
}

// ---- Analytics dashboard types (v3) ----

// DailySeverityPoint is one day of the org-wide severity trend (error/warning/
// info summed across every flow analyzed that day). Drives the stacked-area
// "is my fleet getting healthier over time?" chart. Cloud-only.
type DailySeverityPoint struct {
	Date     string `json:"date"`
	Errors   int    `json:"errors"`
	Warnings int    `json:"warnings"`
	Info     int    `json:"info"`
}

// HealthBucketStub is one 20-point slice of the health-score histogram, so the
// UI can render the distribution the single AvgHealth number hides (e.g. a 70
// average built from a bimodal 40/100 split).
type HealthBucketStub struct {
	Label string `json:"label"` // "0-20", "20-40", ...
	Lo    int    `json:"lo"`
	Hi    int    `json:"hi"`
	Count int    `json:"count"`
}

// FixabilityStub summarizes how much of the current finding load is cheaply
// fixable: Available counts findings carrying a one-click deterministic fix;
// Total is all findings. AutoFixableRules/TotalRules come from the catalog so
// the UI can show "11 of 29 rules are auto-fixable".
type FixabilityStub struct {
	Available        int `json:"available"`
	Total            int `json:"total"`
	AutoFixableRules int `json:"autoFixableRules"`
	TotalRules       int `json:"totalRules"`
}

// WorkflowStub carries the team-triage funnel and resolution health for the
// dashboard's workflow card. Available is false in local mode (no persistent
// triage) so the UI shows a placeholder instead of an all-zero funnel.
type WorkflowStub struct {
	Available     bool           `json:"available"`
	Funnel        map[string]int `json:"funnel"`        // status → count (open/acknowledged/in_progress/resolved/suppressed)
	MttrHours     float64        `json:"mttrHours"`     // mean time to resolve; 0 if none resolved
	ResolvedCount int            `json:"resolvedCount"` // findings contributing to MttrHours
	StaleCount    int            `json:"staleCount"`    // open/acknowledged untouched > 14d
}
