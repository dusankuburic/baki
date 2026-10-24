package interfaces

import "time"

// AnalyticsWindow scopes an analytics read: the authorised flow ids, current and previous window instants (half-open [From, To)), and the IANA zone timestamps are bucketed into days with.
type AnalyticsWindow struct {
	FlowIDs  []string
	From     time.Time
	To       time.Time
	PrevFrom time.Time
	PrevTo   time.Time
	TZ       string
}

// AnalysisSnapshotRow is one flow in scope with its latest persisted analysis (Analyzed=false when it never had one).
type AnalysisSnapshotRow struct {
	FlowID        string
	Name          string
	OwnerID       string
	OrgID         string
	BlockCount    int
	UpdatedAt     time.Time
	Analyzed      bool
	HealthScore   int
	Errors        int
	Warnings      int
	Info          int
	AutoFixable   int
	TotalFindings int
	ByRule        map[string]int
	ByCategory    map[string]int
	ByConfidence  map[string]int
	AnalyzedAt    time.Time
}

// AnalysisHistoryRow is a flow's latest analysis on one caller-local day (Runs = analyses that day), or the pre-window baseline (Day empty).
type AnalysisHistoryRow struct {
	FlowID        string
	Day           string
	HealthScore   int
	Errors        int
	Warnings      int
	Info          int
	AutoFixable   int
	TotalFindings int
	Runs          int
	ByRule        map[string]int
	AnalyzedAt    time.Time
}

type AnalysisHistoryRows struct {
	Baseline []AnalysisHistoryRow
	InWindow []AnalysisHistoryRow
}

type TriageStatusRule struct {
	Status string
	RuleID string
	Count  int
}

type TriageResolvedDay struct {
	Date   string
	RuleID string
	Count  int
}

type TriageRows struct {
	ByStatusRule  []TriageStatusRule
	ResolvedByDay []TriageResolvedDay
	MttrHours     float64
	ResolvedCount int
	StaleCount    int
}

type UsageDailyRow struct {
	Date      string
	TokensIn  int
	TokensOut int
	Calls     int
}

// UsageModelRow: rows are grouped by ServedModel (the model that answered when it differs from Model), so one alias served by two models yields two rows.
type UsageModelRow struct {
	Provider    string
	Model       string
	ServedModel string
	TokensIn    int
	TokensOut   int
	Calls       int
}

// UsageRows: Daily spans the previous and current windows; the breakdowns cover the current one.
type UsageRows struct {
	Daily      []UsageDailyRow
	ByProvider []ProviderUsage
	ByModel    []UsageModelRow
}

type ActivityEventRow struct {
	Action       string
	UserID       string
	ActorName    string
	ResourceType string
	ResourceID   string
	Label        string
	CreatedAt    time.Time
}

type ActionCountRow struct {
	Action string
	Count  int
}

type DateCountRow struct {
	Date  string
	Count int
}

type ActivityRows struct {
	Events   []ActivityEventRow
	ByDay    []DateCountRow
	ByAction []ActionCountRow
}

// SecurityRows: LockedRead reports whether the locked-account count was read (only when asked for; instance-wide).
type SecurityRows struct {
	FailedLogins24h int
	LockedAccounts  int
	LockedRead      bool
}
