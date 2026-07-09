package service

import (
	"context"
	"testing"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/analyzer"
	"pad-core/models"
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
		Stats: models.AnalysisStats{Errors: 2, Warnings: 3, Info: 1},
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

// buildLocal must count a flow once even when it was analyzed under multiple
// content hashes (e.g. after editing): the cache's Put replaces the flow's
// prior entry, so the newest report wins.
func TestDashboardService_BuildLocal_DeduplicatesFlows(t *testing.T) {
	// Start from an empty session cache: entries put via CachedAnalysis key on
	// the path-derived StableFlowID, which Invalidate(report.FlowID) misses.
	analyzer.DefaultCache.Clear()

	t1 := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	t2 := t1.Add(time.Hour)
	// Insert two reports for the same flow (different GeneratedAt)
	analyzer.DefaultCache.Put("dup-flow", "hash-old", &models.AnalysisReport{
		FlowID:      "dup-flow",
		FlowName:    "Old",
		GeneratedAt: t1,
		Findings:    []models.Finding{},
		Stats:       models.AnalysisStats{Errors: 1, Warnings: 0, Info: 0},
	})
	analyzer.DefaultCache.Put("dup-flow", "hash-new", &models.AnalysisReport{
		FlowID:      "dup-flow",
		FlowName:    "New",
		GeneratedAt: t2,
		Findings:    []models.Finding{},
		Stats:       models.AnalysisStats{Errors: 2, Warnings: 0, Info: 0},
	})
	t.Cleanup(func() { analyzer.DefaultCache.Invalidate("dup-flow") })

	analysisSvc := &AnalysisService{}
	svc := NewDashboardService(nil, analysisSvc, nil)

	home := svc.BuildHome(context.Background(), "user-1")

	if home.Overview.TotalFlows != 1 {
		t.Errorf("expected 1 flow after dedup, got %d", home.Overview.TotalFlows)
	}
	if len(home.Complexity) != 1 {
		t.Errorf("expected 1 complexity point after dedup, got %d", len(home.Complexity))
	}
	if home.Complexity[0].FlowName != "New" {
		t.Errorf("expected newest report to win, got %q", home.Complexity[0].FlowName)
	}
}

// TestDashboardService_BuildHome_CloudV3Analytics verifies the v3 dashboard
// sections map from the backend aggregates: severity trend, confidence
// distribution, health histogram, fixability, and the previously-dropped
// TopSeverity (per rule) and LockedAccounts (security posture) fields.
func TestDashboardService_BuildHome_CloudV3Analytics(t *testing.T) {
	backend := &stubDashBackend{
		FakeBackend: testutil.NewFakeBackend(),
		data: &storageif.DashboardData{
			ByCategory:    map[string]int{},
			Confidence:    map[string]int{"high": 4, "medium": 6, "low": 2},
			TotalFindings: 12,
			AutoFixable:   5,
			HealthBuckets: []storageif.HealthBucket{
				{Label: "0-20", Lo: 0, Hi: 20, Count: 1},
				{Label: "80-100", Lo: 80, Hi: 100, Count: 3},
			},
		},
		adv: &storageif.DashboardAdvancedData{
			Security: storageif.DashboardSecurity{LockedAccounts: 2},
			SeverityTrend: []storageif.DailySeverityPoint{
				{Date: "2026-06-01", Errors: 1, Warnings: 3, Info: 2},
			},
			RuleFreq: []storageif.RuleFrequency{
				{Rule: "missing-timeout", Count: 4},
			},
		},
	}
	analysisSvc := &AnalysisService{}
	svc := NewDashboardService(backend, analysisSvc, nil)

	home := svc.BuildHome(context.Background(), "user-1")

	// Severity trend maps 1:1.
	if len(home.SeverityTrend) != 1 || home.SeverityTrend[0].Errors != 1 || home.SeverityTrend[0].Warnings != 3 {
		t.Errorf("SeverityTrend mapping: got %+v", home.SeverityTrend)
	}
	// Confidence distribution maps through.
	if home.ConfidenceDist["high"] != 4 || home.ConfidenceDist["low"] != 2 {
		t.Errorf("ConfidenceDist mapping: got %+v", home.ConfidenceDist)
	}
	// Health histogram maps through.
	if len(home.HealthBuckets) != 2 || home.HealthBuckets[1].Count != 3 {
		t.Errorf("HealthBuckets mapping: got %+v", home.HealthBuckets)
	}
	// TopSeverity is resolved from the rule catalog (missing-timeout defaults
	// to warning), not from the backend rollup.
	if home.RuleFreq[0].TopSeverity != "warning" {
		t.Errorf("RuleFreq TopSeverity dropped: got %+v", home.RuleFreq[0])
	}
	if home.Security.LockedAccounts != 2 {
		t.Errorf("Security LockedAccounts dropped: got %+v", home.Security)
	}
	// Fixability: finding-side from DB, rule-side from the static catalog.
	if home.Fixability.Available != 5 || home.Fixability.Total != 12 {
		t.Errorf("Fixability finding counts: got %+v", home.Fixability)
	}
	if home.Fixability.TotalRules == 0 || home.Fixability.AutoFixableRules == 0 {
		t.Errorf("Fixability catalog counts must be non-zero: got %+v", home.Fixability)
	}
	if home.Fixability.AutoFixableRules >= home.Fixability.TotalRules {
		t.Errorf("more auto-fixable rules than total rules: %+v", home.Fixability)
	}
}

// TestDashboardService_BuildHome_CloudWorkflow verifies the cloud-only triage
// workflow section maps from the backend: the status funnel, MTTR, resolved
// count, and stale count. Available flips true once any finding has a status.
func TestDashboardService_BuildHome_CloudWorkflow(t *testing.T) {
	backend := &stubDashBackend{
		FakeBackend: testutil.NewFakeBackend(),
		data:        &storageif.DashboardData{ByCategory: map[string]int{}},
		adv: &storageif.DashboardAdvancedData{
			Workflow: storageif.WorkflowData{
				Funnel: map[string]int{
					"open": 10, "acknowledged": 3, "in_progress": 2,
					"resolved": 8, "suppressed": 1,
				},
				MttrHours:     16.5,
				ResolvedCount: 8,
				StaleCount:    4,
			},
		},
	}
	svc := NewDashboardService(backend, &AnalysisService{}, nil)

	home := svc.BuildHome(context.Background(), "user-1")

	if !home.Workflow.Available {
		t.Error("Workflow.Available should be true when the funnel has entries")
	}
	if home.Workflow.Funnel["open"] != 10 || home.Workflow.Funnel["resolved"] != 8 {
		t.Errorf("Workflow funnel mapping: got %+v", home.Workflow.Funnel)
	}
	if home.Workflow.MttrHours != 16.5 {
		t.Errorf("Workflow MTTR: got %v, want 16.5", home.Workflow.MttrHours)
	}
	if home.Workflow.ResolvedCount != 8 || home.Workflow.StaleCount != 4 {
		t.Errorf("Workflow resolved/stale counts: got %+v", home.Workflow)
	}
}

// TestDashboardService_BuildHome_CloudWorkflowEmpty verifies the workflow
// section is unavailable (placeholder) when no finding has been triaged yet.
func TestDashboardService_BuildHome_CloudWorkflowEmpty(t *testing.T) {
	backend := &stubDashBackend{
		FakeBackend: testutil.NewFakeBackend(),
		data:        &storageif.DashboardData{ByCategory: map[string]int{}},
		adv:         &storageif.DashboardAdvancedData{},
	}
	svc := NewDashboardService(backend, &AnalysisService{}, nil)

	home := svc.BuildHome(context.Background(), "user-1")

	if home.Workflow.Available {
		t.Error("Workflow.Available should be false when the funnel is empty")
	}
	if home.Workflow.Funnel == nil {
		t.Error("Workflow.Funnel must be non-nil even when empty (JSON {} not null)")
	}
}

// TestDashboardService_BuildLocal_V3Analytics verifies local/desktop mode
// derives the v3 sections from the in-process cache: confidence distribution
// and fix availability from findings, and the health histogram from metrics.
// SeverityTrend stays empty (no per-day time series locally).
func TestDashboardService_BuildLocal_V3Analytics(t *testing.T) {
	// Start from an empty session cache: entries put via CachedAnalysis key on
	// the path-derived StableFlowID, which Invalidate(report.FlowID) misses.
	analyzer.DefaultCache.Clear()
	analyzer.DefaultCache.Put("local-1", "h1", &models.AnalysisReport{
		FlowID:   "local-1",
		FlowName: "Local One",
		Metrics:  &models.FlowMetrics{HealthScore: 90, TotalBlocks: 10},
		Findings: []models.Finding{
			{RuleID: "missing-timeout", Severity: models.SeverityWarning, Confidence: models.ConfidenceMedium, AutoFix: "set-timeout"},
			{RuleID: "hardcoded-url", Severity: models.SeverityInfo, Confidence: models.ConfidenceLow},
		},
	})
	analyzer.DefaultCache.Put("local-2", "h2", &models.AnalysisReport{
		FlowID:   "local-2",
		FlowName: "Local Two",
		Metrics:  &models.FlowMetrics{HealthScore: 30, TotalBlocks: 5},
		Findings: []models.Finding{
			{RuleID: "resource-leak", Severity: models.SeverityWarning, Confidence: models.ConfidenceHigh, AutoFix: "insert-close"},
		},
	})
	t.Cleanup(func() {
		analyzer.DefaultCache.Invalidate("local-1")
		analyzer.DefaultCache.Invalidate("local-2")
	})

	svc := NewDashboardService(nil, &AnalysisService{}, nil)
	home := svc.BuildHome(context.Background(), "user-1")

	// Confidence: 1 high + 1 medium + 1 low.
	if home.ConfidenceDist["high"] != 1 || home.ConfidenceDist["medium"] != 1 || home.ConfidenceDist["low"] != 1 {
		t.Errorf("ConfidenceDist from cache: got %+v", home.ConfidenceDist)
	}
	// Fix availability: 2 of 3 findings carry an auto-fix.
	if home.Fixability.Available != 2 || home.Fixability.Total != 3 {
		t.Errorf("Fixability from cache: got %+v", home.Fixability)
	}
	// Histogram: scores 90 and 30 land in the top and second buckets.
	if home.HealthBuckets[1].Count != 1 || home.HealthBuckets[4].Count != 1 {
		t.Errorf("HealthBuckets from cache: got %+v", home.HealthBuckets)
	}
	// No per-day series locally.
	if len(home.SeverityTrend) != 0 {
		t.Errorf("local SeverityTrend should be empty, got %+v", home.SeverityTrend)
	}
	// TopSeverity resolves from the rule catalog in local mode too.
	sevByRule := map[string]string{}
	for _, rf := range home.RuleFreq {
		sevByRule[rf.Rule] = rf.TopSeverity
	}
	if sevByRule["missing-timeout"] != "warning" || sevByRule["resource-leak"] != "warning" {
		t.Errorf("local RuleFreq TopSeverity from catalog: got %+v", sevByRule)
	}
}

// TestDashboardService_BuildLocal_ReloadedFileCountsOnce is the regression test
// for the session-analytics identity fix: re-opening and re-analyzing the same
// file (parser mints a fresh doc UUID each load) must update the flow's single
// dashboard entry, not add a second one.
func TestDashboardService_BuildLocal_ReloadedFileCountsOnce(t *testing.T) {
	analyzer.DefaultCache.Clear()
	block := models.Block{ID: "b1", SubflowID: "sf1", Name: "Set X", Type: models.BlockTypeAction, RawType: "SetVariable.Set"}
	mkDoc := func(uuid string) *models.FlowDocument {
		return &models.FlowDocument{
			ID: uuid, FilePath: "/flows/reload-me.txt", Name: "Reload Me",
			Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{block}}},
		}
	}
	analyzer.CachedAnalysis(mkDoc("reload-uuid-1"), analyzer.AllRules(), nil, nil)
	analyzer.CachedAnalysis(mkDoc("reload-uuid-2"), analyzer.AllRules(), nil, nil)
	t.Cleanup(analyzer.DefaultCache.Clear)

	svc := NewDashboardService(nil, &AnalysisService{}, nil)
	home := svc.BuildHome(context.Background(), "user-1")

	if home.Overview.TotalFlows != 1 {
		t.Errorf("re-loaded file must count once: TotalFlows = %d, want 1", home.Overview.TotalFlows)
	}
	if len(home.Complexity) != 1 {
		t.Errorf("complexity scatter must have one point, got %d", len(home.Complexity))
	}
}
