package service

import (
	"context"
	"testing"
	"time"

	"pad-analyzer/internal/models"
	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// stubDashBackend embeds FakeBackend (to satisfy the large StorageBackend
// interface) and overrides only FlowDashboardData so we can exercise the
// cloud-path mapping without a real Postgres.
type stubDashBackend struct {
	*testutil.FakeBackend
	data *storageif.DashboardData
}

func (s *stubDashBackend) FlowDashboardData(_ context.Context, _ string, _ int) (*storageif.DashboardData, error) {
	return s.data, nil
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
	svc := NewDashboardService(backend, nil)

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
	svc := NewDashboardService(nil, nil)

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
	svc := NewDashboardService(nil, nil)
	svc.RecordAnalysis(context.Background(),
		&models.FlowDocument{ID: "f1"},
		&models.AnalysisReport{Stats: models.AnalysisStats{Errors: 1}},
	)
}
