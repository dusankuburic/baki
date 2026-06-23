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
}
