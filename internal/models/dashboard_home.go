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
	TokenUsage  []DailyTokenUsage    `json:"tokenUsage"`  // gap-filled 14-day series in cloud; empty in local mode
	RecentFlows []RecentFlowStub     `json:"recentFlows"`
	Findings    DashboardFindingsAgg `json:"findings"`
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
