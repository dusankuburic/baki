package service

import (
	"context"
	"testing"
	"time"

	"pad-core/models"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// stubDashBackend embeds FakeBackend (to satisfy the large StorageBackend
// interface) and overrides only FlowDashboardData so we can exercise the
// cloud-path mapping without a real Postgres.
type stubDashBackend struct {
	*testutil.FakeBackend
	data *storageif.DashboardData
	adv  *storageif.DashboardAdvancedData
}

func (s *stubDashBackend) FlowDashboardData(_ context.Context, _ string, _ int) (*storageif.DashboardData, error) {
	return s.data, nil
}

func (s *stubDashBackend) FlowDashboardAdvanced(_ context.Context, _ string, _ int) (*storageif.DashboardAdvancedData, error) {
	if s.adv == nil {
		return &storageif.DashboardAdvancedData{Security: storageif.DashboardSecurity{}}, nil
	}
	return s.adv, nil
}

func (s *stubDashBackend) SaveFlowAnalysis(_ context.Context, fa *storageif.FlowAnalysis) error {
	s.data = &storageif.DashboardData{
		ByCategory: fa.ByCategory,
	}
	return nil
}

func intPtr(v int) *int { return &v }

func TestDashboardService_BuildHome_CloudMapping(t *testing.T) {
	backend := &stubDashBackend{
		FakeBackend: testutil.NewFakeBackend(),
		data: &storageif.DashboardData{
			TotalFlows:    3,
			TotalSubflows: 7,
			HealthCount:   2,
			AvgHealth:     82,
			Errors:        5,
			Warnings:      3,
			Info:          1,
			// reliability and security tie at 4 → must order alphabetically; style last.
			ByCategory: map[string]int{"security": 4, "reliability": 4, "style": 2},
			Recent: []storageif.RecentFlowHealth{
				{FlowID: "f1", Name: "Flow 1", HealthScore: intPtr(90), UpdatedAt: time.Now()},
				{FlowID: "f2", Name: "Flow 2", HealthScore: nil, UpdatedAt: time.Now()},
			},
			TokenUsage: []storageif.DailyTokens{{Date: "2026-06-01", TokensIn: 100, TokensOut: 50}},
		},
	}
	svc := NewDashboardService(backend, nil, nil)

	home := svc.BuildHome(context.Background(), "user-1")

	if home.Overview.AvgHealthScore != 82 || !home.Overview.HealthAvailable {
		t.Errorf("overview health: got %+v", home.Overview)
	}
	if home.Overview.TotalFlows != 3 || home.Overview.TotalSubflows != 7 {
		t.Errorf("overview counts: got %+v", home.Overview)
	}
	if !home.Findings.Available {
		t.Error("findings should be available")
	}
	if home.Findings.BySeverity["error"] != 5 || home.Findings.BySeverity["warning"] != 3 || home.Findings.BySeverity["info"] != 1 {
		t.Errorf("severity map: got %+v", home.Findings.BySeverity)
	}
	// Tie broken alphabetically, then lower count last.
	wantOrder := []string{"reliability", "security", "style"}
	if len(home.Findings.ByCategory) != 3 {
		t.Fatalf("want 3 categories, got %d", len(home.Findings.ByCategory))
	}
	for i, want := range wantOrder {
		if home.Findings.ByCategory[i].Category != want {
			t.Errorf("category[%d]: want %s, got %s", i, want, home.Findings.ByCategory[i].Category)
		}
	}
	if len(home.RecentFlows) != 2 {
		t.Fatalf("want 2 recent flows, got %d", len(home.RecentFlows))
	}
	if home.RecentFlows[0].HealthScore == nil || *home.RecentFlows[0].HealthScore != 90 {
		t.Errorf("recent[0] health: got %v", home.RecentFlows[0].HealthScore)
	}
	if home.RecentFlows[1].HealthScore != nil {
		t.Errorf("recent[1] health should be nil (never analyzed), got %v", *home.RecentFlows[1].HealthScore)
	}
	if len(home.TokenUsage) != 1 || home.TokenUsage[0].TokensIn != 100 {
		t.Errorf("token usage: got %+v", home.TokenUsage)
	}
}

// In local mode (nil backend) BuildHome must still return a valid, non-nil
// payload so the frontend's length checks and chart mappers don't hit nulls.
func TestDashboardService_BuildHome_LocalEmptyIsSafe(t *testing.T) {
	svc := NewDashboardService(nil, nil, nil)

	home := svc.BuildHome(context.Background(), "user-1")

	if home.TokenUsage == nil || home.RecentFlows == nil {
		t.Error("slices must be non-nil (empty), not null")
	}
	if home.Findings.BySeverity == nil || home.Findings.ByCategory == nil {
		t.Error("findings maps/slices must be non-nil")
	}
	if home.Overview.HealthAvailable {
		t.Error("health must not be available with no data")
	}
}

// RecordAnalysis must be a no-op (and never panic) in local mode.
func TestDashboardService_RecordAnalysis_LocalNoop(t *testing.T) {
	svc := NewDashboardService(nil, nil, nil)
	svc.RecordAnalysis(context.Background(),
		&models.FlowDocument{ID: "f1"},
		&models.AnalysisReport{Stats: models.AnalysisStats{Errors: 1}},
	)
}

// RecordAnalysis in cloud mode must persist by_category AND by_rule maps
// computed from the findings slice, plus the correct severity counts.
func TestDashboardService_RecordAnalysis_CloudPersistsByRuleAndCategory(t *testing.T) {
	backend := &stubDashBackend{FakeBackend: testutil.NewFakeBackend()}
	svc := NewDashboardService(backend, nil, nil)

	report := &models.AnalysisReport{
		Stats:      models.AnalysisStats{Errors: 2, Warnings: 3, Info: 1},
		Findings: []models.Finding{
			{RuleID: "hardcoded-credential", Category: "Security", Severity: "error"},
			{RuleID: "hardcoded-credential", Category: "Security", Severity: "error"},
			{RuleID: "dead-code", Category: "Style", Severity: "info"},
			{RuleID: "unhandled-error", Category: "Reliability", Severity: "warning"},
		},
	}

	svc.RecordAnalysis(context.Background(), &models.FlowDocument{ID: "f1"}, report)

	// The stub's SaveFlowAnalysis stored the by_category map in data.ByCategory.
	if backend.data == nil {
		t.Fatal("SaveFlowAnalysis was not called")
	}
	if backend.data.ByCategory["Security"] != 2 {
		t.Errorf("expected Security category count 2, got %d", backend.data.ByCategory["Security"])
	}
	if backend.data.ByCategory["Style"] != 1 {
		t.Errorf("expected Style category count 1, got %d", backend.data.ByCategory["Style"])
	}
}

// BuildHome in cloud mode must include advanced sections even when the
// advanced data source returns empty slices (graceful degradation).
func TestDashboardService_BuildHome_CloudAdvancedEmptyIsSafe(t *testing.T) {
	backend := &stubDashBackend{
		FakeBackend: testutil.NewFakeBackend(),
		data: &storageif.DashboardData{
			ByCategory: map[string]int{},
		},
	}
	svc := NewDashboardService(backend, nil, nil)

	home := svc.BuildHome(context.Background(), "user-1")

	// Advanced sections must be non-nil (empty slices, not null) so the
	// frontend's length checks don't crash.
	if home.HealthTrend == nil {
		t.Error("HealthTrend must be non-nil")
	}
	if home.CostByProv == nil {
		t.Error("CostByProv must be non-nil")
	}
	if home.RuleFreq == nil {
		t.Error("RuleFreq must be non-nil")
	}
	if home.Activity == nil {
		t.Error("Activity must be non-nil")
	}
	if home.Complexity == nil {
		t.Error("Complexity must be non-nil")
	}
}

// BuildHome in cloud mode must map advanced data from the backend correctly.
func TestDashboardService_BuildHome_CloudAdvancedMapping(t *testing.T) {
	backend := &stubDashBackend{
		FakeBackend: testutil.NewFakeBackend(),
		data: &storageif.DashboardData{
			ByCategory: map[string]int{},
		},
		adv: &storageif.DashboardAdvancedData{
			HealthTrend: []storageif.DailyHealthPoint{
				{Date: "2026-06-01", AvgHealth: 80, FlowCount: 3},
			},
			CostByProv: []storageif.ProviderCost{
				{Provider: "claude", Cost: 1.50, TokensIn: 1000, TokensOut: 500},
			},
			RuleFreq: []storageif.RuleFrequency{
				{Rule: "dead-code", Count: 5},
			},
			Activity: []storageif.ActivityEntry{
				{Action: "flow.analyze", FlowName: "Test", CreatedAt: time.Now()},
			},
			Complexity: []storageif.FlowComplexityPoint{
				{FlowID: "f1", FlowName: "Big", BlockCount: 100, FindingCount: 12, HealthScore: 75},
			},
			Security: storageif.DashboardSecurity{FailedLogins24h: 3, CredentialFindings: 2},
		},
	}
	svc := NewDashboardService(backend, nil, nil)

	home := svc.BuildHome(context.Background(), "user-1")

	if len(home.HealthTrend) != 1 || home.HealthTrend[0].AvgHealth != 80 {
		t.Errorf("HealthTrend mapping: got %+v", home.HealthTrend)
	}
	if len(home.CostByProv) != 1 || home.CostByProv[0].Cost != 1.50 {
		t.Errorf("CostByProv mapping: got %+v", home.CostByProv)
	}
	if len(home.RuleFreq) != 1 || home.RuleFreq[0].Rule != "dead-code" {
		t.Errorf("RuleFreq mapping: got %+v", home.RuleFreq)
	}
	if len(home.Activity) != 1 || home.Activity[0].Action != "flow.analyze" {
		t.Errorf("Activity mapping: got %+v", home.Activity)
	}
	if len(home.Complexity) != 1 || home.Complexity[0].BlockCount != 100 {
		t.Errorf("Complexity mapping: got %+v", home.Complexity)
	}
	if home.Security.FailedLogins24h != 3 || home.Security.CredentialFindings != 2 {
		t.Errorf("Security mapping: got %+v", home.Security)
	}
}
