package models

import "time"

// DashboardAnalyticsData is the payload for GET /api/dashboard/analytics.
// Every section is computed for the same scope and window (Applied / Range)
// so the numbers agree with each other; a section whose source failed is
// named in Partial and left empty rather than failing the request.
type DashboardAnalyticsData struct {
	Applied     AnalyticsApplied  `json:"applied"`
	Range       AnalyticsRange    `json:"range"`
	Summary     AnalyticsSummary  `json:"summary"`
	Trend       AnalyticsTrend    `json:"trend"`
	Findings    AnalyticsFindings `json:"findings"`
	Flows       AnalyticsFlows    `json:"flows"`
	Triage      AnalyticsTriage   `json:"triage"`
	AI          AnalyticsAI       `json:"ai"`
	Activity    AnalyticsActivity `json:"activity"`
	Security    AnalyticsSecurity `json:"security"`
	Facets      AnalyticsFacets   `json:"facets"`
	Partial     []string          `json:"partial"`
	GeneratedAt time.Time         `json:"generatedAt"`
}

// AnalyticsApplied echoes the filters the payload was computed for (flows that
// the caller may not see are dropped silently).
type AnalyticsApplied struct {
	Scope    string   `json:"scope"`
	OrgID    string   `json:"orgId,omitempty"`
	Flows    []string `json:"flows"`
	Severity []string `json:"severity"`
	Category []string `json:"category"`
	Rule     []string `json:"rule"`
	Compare  bool     `json:"compare"`
	TZ       string   `json:"tz"`
	// Derived is true when a severity / category / rule filter is active: the
	// findings counts then come from each flow's by_rule rollup mapped through
	// the current rule catalog instead of the stored severity totals.
	Derived bool `json:"derived"`
}

type AnalyticsRange struct {
	Preset   string `json:"preset"`
	From     string `json:"from"`
	To       string `json:"to"`
	Days     int    `json:"days"`
	PrevFrom string `json:"prevFrom,omitempty"`
	PrevTo   string `json:"prevTo,omitempty"`
}

// StatValue is one headline number: its value now (or as of the window's last
// day), the same number for the previous period when comparing, and a
// sparkline of at most 12 buckets across the window.
type StatValue struct {
	Available bool      `json:"available"`
	Value     float64   `json:"value"`
	Prev      *float64  `json:"prev"`
	Spark     []float64 `json:"spark"`
}

type AnalyticsSummary struct {
	AsOf          string    `json:"asOf"`
	FlowsInScope  int       `json:"flowsInScope"`
	Unanalyzed    int       `json:"unanalyzed"`
	AvgHealth     StatValue `json:"avgHealth"`
	OpenFindings  StatValue `json:"openFindings"`
	Errors        StatValue `json:"errors"`
	Warnings      StatValue `json:"warnings"`
	Info          StatValue `json:"info"`
	AutoFixable   StatValue `json:"autoFixable"`
	AnalysesRun   StatValue `json:"analysesRun"`
	FlowsAnalyzed StatValue `json:"flowsAnalyzed"`
	Resolved      StatValue `json:"resolved"`
	AISpend       StatValue `json:"aiSpend"`
}

// AnalyticsTrendPoint is the fleet's state as of one day: every flow carries
// its latest analysis forward, so a day without runs repeats the prior state
// instead of reading as zero. AvgHealth is nil until any flow has a snapshot.
type AnalyticsTrendPoint struct {
	Date        string `json:"date"`
	AvgHealth   *int   `json:"avgHealth"`
	FlowCount   int    `json:"flowCount"`
	Runs        int    `json:"runs"`
	Findings    int    `json:"findings"`
	Errors      int    `json:"errors"`
	Warnings    int    `json:"warnings"`
	Info        int    `json:"info"`
	AutoFixable int    `json:"autoFixable"`
}

type AnalyticsTrend struct {
	Points     []AnalyticsTrendPoint `json:"points"`
	PrevPoints []AnalyticsTrendPoint `json:"prevPoints"`
}

type RuleCount struct {
	Rule        string `json:"rule"`
	Name        string `json:"name"`
	Category    string `json:"category"`
	Severity    string `json:"severity"`
	AutoFixable bool   `json:"autoFixable"`
	InCatalog   bool   `json:"inCatalog"`
	Count       int    `json:"count"`
	Flows       int    `json:"flows"`
}

type CategoryCount struct {
	Category string `json:"category"`
	Count    int    `json:"count"`
	Errors   int    `json:"errors"`
	Warnings int    `json:"warnings"`
	Info     int    `json:"info"`
}

type AnalyticsFindings struct {
	BySeverity   map[string]int  `json:"bySeverity"`
	ByCategory   []CategoryCount `json:"byCategory"`
	ByRule       []RuleCount     `json:"byRule"`
	ByConfidence map[string]int  `json:"byConfidence"`
	Fixability   FixabilityStub  `json:"fixability"`
}

type FlowPoint struct {
	FlowID     string    `json:"flowId"`
	Name       string    `json:"name"`
	OwnerID    string    `json:"ownerId"`
	BlockCount int       `json:"blockCount"`
	Findings   int       `json:"findings"`
	Errors     int       `json:"errors"`
	Warnings   int       `json:"warnings"`
	Info       int       `json:"info"`
	Health     int       `json:"health"`
	AnalyzedAt time.Time `json:"analyzedAt"`
}

type FlowMover struct {
	FlowID     string `json:"flowId"`
	Name       string `json:"name"`
	HealthFrom *int   `json:"healthFrom"`
	HealthTo   *int   `json:"healthTo"`
	Delta      int    `json:"delta"`
}

type AnalyticsFlows struct {
	HealthBuckets []HealthBucketStub `json:"healthBuckets"`
	Complexity    []FlowPoint        `json:"complexity"`
	Worst         []FlowPoint        `json:"worst"`
	Movers        []FlowMover        `json:"movers"`
}

type DateCount struct {
	Date  string `json:"date"`
	Count int    `json:"count"`
}

type AnalyticsTriage struct {
	Available     bool           `json:"available"`
	Funnel        map[string]int `json:"funnel"`
	Backlog       int            `json:"backlog"`
	MttrHours     float64        `json:"mttrHours"`
	ResolvedCount int            `json:"resolvedCount"`
	StaleCount    int            `json:"staleCount"`
	ResolvedByDay []DateCount    `json:"resolvedByDay"`
}

type AIDailyPoint struct {
	Date      string  `json:"date"`
	TokensIn  int     `json:"tokensIn"`
	TokensOut int     `json:"tokensOut"`
	Cost      float64 `json:"cost"`
	Calls     int     `json:"calls"`
}

// ModelUsage is one row of the by-model breakdown. Model is what the caller
// asked for (for Azure AI Foundry a deployment name or agent); ServedModel is
// the model that actually answered when it differs. Rows are grouped by the
// served model, so one deployment routed to two models yields two rows.
type ModelUsage struct {
	Provider    string  `json:"provider"`
	Model       string  `json:"model"`
	ServedModel string  `json:"servedModel,omitempty"`
	TokensIn    int     `json:"tokensIn"`
	TokensOut   int     `json:"tokensOut"`
	Calls       int     `json:"calls"`
	Cost        float64 `json:"cost"`
}

// AnalyticsAI is AI usage for the caller's account and/or the scoped orgs.
// usage_metrics has no flow id, so FlowScoped is always false and the UI says
// so instead of pretending the flows filter applied.
type AnalyticsAI struct {
	Scoped      string             `json:"scoped"`
	FlowScoped  bool               `json:"flowScoped"`
	Daily       []AIDailyPoint     `json:"daily"`
	ByProvider  []ProviderCostStub `json:"byProvider"`
	ByModel     []ModelUsage       `json:"byModel"`
	TotalCost   float64            `json:"totalCost"`
	PrevCost    *float64           `json:"prevCost"`
	TotalTokens int                `json:"totalTokens"`
}

type ActivityActor struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	IsMe bool   `json:"isMe"`
}

type ActivityEvent struct {
	Action       string        `json:"action"`
	Actor        ActivityActor `json:"actor"`
	ResourceType string        `json:"resourceType"`
	ResourceID   string        `json:"resourceId"`
	Label        string        `json:"label"`
	CreatedAt    time.Time     `json:"createdAt"`
}

type ActionCount struct {
	Action string `json:"action"`
	Count  int    `json:"count"`
}

type AnalyticsActivity struct {
	Events   []ActivityEvent `json:"events"`
	ByDay    []DateCount     `json:"byDay"`
	ByAction []ActionCount   `json:"byAction"`
}

// AnalyticsSecurity: LockedAccounts is an instance-wide count and is only
// present for instance admins.
type AnalyticsSecurity struct {
	Available          bool `json:"available"`
	FailedLogins24h    int  `json:"failedLogins24h"`
	SecurityFindings   int  `json:"securityFindings"`
	CredentialFindings int  `json:"credentialFindings"`
	LockedAccounts     *int `json:"lockedAccounts"`
}

type FacetCount struct {
	Key   string `json:"key"`
	Label string `json:"label"`
	Count int    `json:"count"`
}

type FacetFlow struct {
	ID         string     `json:"id"`
	Name       string     `json:"name"`
	Health     *int       `json:"health"`
	Findings   int        `json:"findings"`
	AnalyzedAt *time.Time `json:"analyzedAt"`
	OrgID      string     `json:"orgId,omitempty"`
}

// AnalyticsFacets are the chip counts: each dimension is counted under the
// other filters (so a chip shows what picking it would leave), and Flows is
// the picker's list (every flow in scope, ignoring the flows filter).
type AnalyticsFacets struct {
	Severity       []FacetCount `json:"severity"`
	Category       []FacetCount `json:"category"`
	Rule           []FacetCount `json:"rule"`
	Flows          []FacetFlow  `json:"flows"`
	FlowsTruncated bool         `json:"flowsTruncated"`
}
