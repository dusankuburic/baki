package service

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/analyzer"
	"pad-core/models"
)

// anaStubBackend serves canned analytics rows. Snapshots are filtered by the
// requested ids so the flows-filter intersection and the scope-wide facet read
// are both observable.
type anaStubBackend struct {
	*testutil.FakeBackend
	mu          sync.Mutex
	snaps       []storageif.AnalysisSnapshotRow
	hist        *storageif.AnalysisHistoryRows
	triage      *storageif.TriageRows
	usage       *storageif.UsageRows
	activity    *storageif.ActivityRows
	security    *storageif.SecurityRows
	fail        map[string]error
	snapCalls   [][]string
	histWindows []storageif.AnalyticsWindow
	usageModes  []string
}

func newAnaStub() *anaStubBackend {
	return &anaStubBackend{FakeBackend: testutil.NewFakeBackend(), fail: map[string]error{}}
}

func (b *anaStubBackend) LoadAnalysisSnapshots(_ context.Context, ids []string) ([]storageif.AnalysisSnapshotRow, error) {
	b.mu.Lock()
	b.snapCalls = append(b.snapCalls, append([]string(nil), ids...))
	b.mu.Unlock()
	if err := b.fail["findings"]; err != nil {
		return nil, err
	}
	in := map[string]bool{}
	for _, id := range ids {
		in[id] = true
	}
	out := []storageif.AnalysisSnapshotRow{}
	for _, r := range b.snaps {
		if in[r.FlowID] {
			out = append(out, r)
		}
	}
	return out, nil
}

func (b *anaStubBackend) LoadAnalysisHistory(_ context.Context, w storageif.AnalyticsWindow) (*storageif.AnalysisHistoryRows, error) {
	b.mu.Lock()
	b.histWindows = append(b.histWindows, w)
	b.mu.Unlock()
	if err := b.fail["trend"]; err != nil {
		return nil, err
	}
	if b.hist == nil {
		return &storageif.AnalysisHistoryRows{}, nil
	}
	return b.hist, nil
}

func (b *anaStubBackend) LoadTriageAnalytics(_ context.Context, _ storageif.AnalyticsWindow) (*storageif.TriageRows, error) {
	if err := b.fail["triage"]; err != nil {
		return nil, err
	}
	if b.triage == nil {
		return &storageif.TriageRows{}, nil
	}
	return b.triage, nil
}

func (b *anaStubBackend) LoadUsageAnalytics(_ context.Context, _ string, _ []string, mode string, _ storageif.AnalyticsWindow) (*storageif.UsageRows, error) {
	b.mu.Lock()
	b.usageModes = append(b.usageModes, mode)
	b.mu.Unlock()
	if err := b.fail["ai"]; err != nil {
		return nil, err
	}
	if b.usage == nil {
		return &storageif.UsageRows{}, nil
	}
	return b.usage, nil
}

func (b *anaStubBackend) LoadActivityAnalytics(_ context.Context, _ string, _ storageif.AnalyticsWindow, _ int) (*storageif.ActivityRows, error) {
	if err := b.fail["activity"]; err != nil {
		return nil, err
	}
	if b.activity == nil {
		return &storageif.ActivityRows{}, nil
	}
	return b.activity, nil
}

func (b *anaStubBackend) LoadSecurityAnalytics(_ context.Context, _, _ string, _ bool) (*storageif.SecurityRows, error) {
	if err := b.fail["security"]; err != nil {
		return nil, err
	}
	if b.security == nil {
		return &storageif.SecurityRows{}, nil
	}
	return b.security, nil
}

type stubScope struct {
	mu        sync.Mutex
	ids       []string
	orgIDs    []string
	err       error
	calls     int
	lastScope LibraryScope
	lastOrg   string
}

func (s *stubScope) ScopeFlowIDs(_ context.Context, _ string, orgID string, scope LibraryScope, _ int) ([]string, []string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls++
	s.lastScope, s.lastOrg = scope, orgID
	if s.err != nil {
		return nil, nil, s.err
	}
	return append([]string(nil), s.ids...), append([]string(nil), s.orgIDs...), nil
}

func (s *stubScope) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

// anaQuery is a preset window of n days ending today (UTC), comparing.
func anaQuery(days int) AnalyticsQuery {
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	return AnalyticsQuery{
		Preset: fmt.Sprintf("%dd", days), From: today.AddDate(0, 0, -(days - 1)), To: today.AddDate(0, 0, 1), Days: days,
		Loc: time.UTC, TZ: "UTC", Scope: "all", Compare: true,
	}
}

func dayOf(q AnalyticsQuery, offset int) string {
	return q.From.AddDate(0, 0, offset).Format(analyticsDay)
}

func snap(id string, health, errs, warns, infos int, byRule map[string]int) storageif.AnalysisSnapshotRow {
	total := 0
	for _, n := range byRule {
		total += n
	}
	return storageif.AnalysisSnapshotRow{
		FlowID: id, Name: "Flow " + id, OwnerID: "user-1", BlockCount: 12, UpdatedAt: time.Now(), Analyzed: true,
		HealthScore: health, Errors: errs, Warnings: warns, Info: infos, TotalFindings: total, ByRule: byRule, AnalyzedAt: time.Now(),
	}
}

// pickRule returns a built-in rule with the wanted default severity, and one
// that carries an auto-fix when fixable is set.
func pickRule(t *testing.T, severity string, fixable bool) analyzer.Rule {
	t.Helper()
	for _, r := range analyzer.AllRules() {
		if string(r.DefaultSeverity()) != severity {
			continue
		}
		if fixable && analyzer.RuleAutoFix(r.ID()) == "" {
			continue
		}
		return r
	}
	t.Fatalf("no built-in %s rule (fixable=%v)", severity, fixable)
	return nil
}

func intp(v *int) string {
	if v == nil {
		return "nil"
	}
	return fmt.Sprint(*v)
}

func TestBuildAnalytics_CarryForwardTrend(t *testing.T) {
	q := anaQuery(7)
	be := newAnaStub()
	be.snaps = []storageif.AnalysisSnapshotRow{
		snap("A", 80, 2, 0, 0, map[string]int{"r1": 2}),
		snap("B", 60, 0, 1, 0, map[string]int{"r2": 1}),
	}
	be.hist = &storageif.AnalysisHistoryRows{InWindow: []storageif.AnalysisHistoryRow{
		{FlowID: "A", Day: dayOf(q, 1), HealthScore: 80, Errors: 2, TotalFindings: 2, Runs: 1, ByRule: map[string]int{"r1": 2}},
		{FlowID: "B", Day: dayOf(q, 3), HealthScore: 60, Warnings: 1, TotalFindings: 1, Runs: 2, ByRule: map[string]int{"r2": 1}},
	}}
	svc := NewDashboardService(be, nil, nil, nil)
	svc.scope = &stubScope{ids: []string{"A", "B"}}

	out, err := svc.BuildAnalytics(context.Background(), "user-1", AnalyticsCaller{}, q)
	if err != nil {
		t.Fatalf("BuildAnalytics: %v", err)
	}
	pts := out.Trend.Points
	if len(pts) != 7 || len(out.Trend.PrevPoints) != 7 {
		t.Fatalf("points = %d/%d, want 7 current and 7 previous", len(pts), len(out.Trend.PrevPoints))
	}
	if pts[0].AvgHealth != nil || pts[0].FlowCount != 0 {
		t.Errorf("day 0 (before any analysis) = %s/%d flows, want nil/0 — never a fake zero", intp(pts[0].AvgHealth), pts[0].FlowCount)
	}
	if intp(pts[1].AvgHealth) != "80" || pts[1].Findings != 2 || pts[1].Runs != 1 {
		t.Errorf("day 1 = %+v, want health 80, 2 findings, 1 run", pts[1])
	}
	if intp(pts[2].AvgHealth) != "80" || pts[2].Runs != 0 || pts[2].FlowCount != 1 {
		t.Errorf("day 2 must carry day 1 forward: %+v", pts[2])
	}
	if intp(pts[3].AvgHealth) != "70" || pts[3].FlowCount != 2 || pts[3].Findings != 3 || pts[3].Runs != 2 {
		t.Errorf("day 3 = %+v, want avg 70 over 2 flows, 3 findings, 2 runs", pts[3])
	}
	if last := pts[6]; last.Date != dayOf(q, 6) || intp(last.AvgHealth) != "70" || last.Findings != 3 {
		t.Errorf("today = %+v, want the live snapshot aggregate (70 / 3)", last)
	}
	if !out.Summary.AvgHealth.Available || out.Summary.AvgHealth.Value != 70 || out.Summary.AvgHealth.Prev != nil {
		t.Errorf("AvgHealth = %+v, want available 70 with no previous value", out.Summary.AvgHealth)
	}
	if out.Summary.OpenFindings.Value != 3 || out.Summary.Errors.Value != 2 || out.Summary.Warnings.Value != 1 {
		t.Errorf("findings tiles = %v/%v/%v", out.Summary.OpenFindings.Value, out.Summary.Errors.Value, out.Summary.Warnings.Value)
	}
	if out.Summary.AnalysesRun.Value != 3 || out.Summary.AnalysesRun.Prev == nil || *out.Summary.AnalysesRun.Prev != 0 {
		t.Errorf("AnalysesRun = %+v, want 3 with previous 0", out.Summary.AnalysesRun)
	}
	if len(out.Summary.AvgHealth.Spark) != 6 || out.Summary.AvgHealth.Spark[0] != 80 {
		t.Errorf("AvgHealth spark = %v, want 6 buckets starting at 80 (the empty day is skipped)", out.Summary.AvgHealth.Spark)
	}
	if out.Summary.FlowsInScope != 2 || out.Summary.Unanalyzed != 0 || out.Summary.AsOf != dayOf(q, 6) {
		t.Errorf("summary scope = %d/%d as of %s", out.Summary.FlowsInScope, out.Summary.Unanalyzed, out.Summary.AsOf)
	}
	if out.Range.From != dayOf(q, 0) || out.Range.To != dayOf(q, 6) || out.Range.PrevTo != dayOf(q, -1) || out.Range.PrevFrom != q.PrevFrom().Format(analyticsDay) {
		t.Errorf("range = %+v", out.Range)
	}
	if out.Applied.Derived {
		t.Error("no finding filter → counts are the stored totals, not derived")
	}
	if len(be.histWindows) != 1 || !be.histWindows[0].PrevFrom.Equal(q.PrevFrom()) {
		t.Errorf("history window = %+v, want PrevFrom %v", be.histWindows, q.PrevFrom())
	}
}

func TestBuildAnalytics_BaselineFeedsPreviousPeriodAndMovers(t *testing.T) {
	q := anaQuery(7)
	be := newAnaStub()
	be.snaps = []storageif.AnalysisSnapshotRow{snap("A", 80, 0, 0, 0, map[string]int{})}
	be.hist = &storageif.AnalysisHistoryRows{Baseline: []storageif.AnalysisHistoryRow{{FlowID: "A", HealthScore: 50, Runs: 1, ByRule: map[string]int{}}}}
	svc := NewDashboardService(be, nil, nil, nil)
	svc.scope = &stubScope{ids: []string{"A"}}

	out, err := svc.BuildAnalytics(context.Background(), "user-1", AnalyticsCaller{}, q)
	if err != nil {
		t.Fatalf("BuildAnalytics: %v", err)
	}
	for i, p := range out.Trend.PrevPoints {
		if intp(p.AvgHealth) != "50" {
			t.Errorf("previous day %d = %s, want the carried baseline 50", i, intp(p.AvgHealth))
		}
	}
	if intp(out.Trend.Points[0].AvgHealth) != "50" || intp(out.Trend.Points[6].AvgHealth) != "80" {
		t.Errorf("trend = %s … %s, want 50 carried until today's snapshot 80", intp(out.Trend.Points[0].AvgHealth), intp(out.Trend.Points[6].AvgHealth))
	}
	if out.Summary.AvgHealth.Value != 80 || out.Summary.AvgHealth.Prev == nil || *out.Summary.AvgHealth.Prev != 50 {
		t.Errorf("AvgHealth = %+v, want 80 vs previous 50", out.Summary.AvgHealth)
	}
	if len(out.Flows.Movers) != 1 || out.Flows.Movers[0].Delta != 30 || intp(out.Flows.Movers[0].HealthFrom) != "50" || intp(out.Flows.Movers[0].HealthTo) != "80" {
		t.Errorf("movers = %+v, want A 50 → 80 (+30)", out.Flows.Movers)
	}
	if out.Flows.Movers[0].Name != "Flow A" {
		t.Errorf("mover name = %q", out.Flows.Movers[0].Name)
	}

	// Without comparison there is no previous period and no movers.
	q.Compare = false
	out, err = svc.BuildAnalytics(context.Background(), "user-1", AnalyticsCaller{}, q)
	if err != nil {
		t.Fatalf("BuildAnalytics (no compare): %v", err)
	}
	if len(out.Trend.PrevPoints) != 0 || len(out.Flows.Movers) != 0 || out.Summary.AvgHealth.Prev != nil || out.Range.PrevFrom != "" {
		t.Errorf("compare=false must drop the previous period: prev=%d movers=%d", len(out.Trend.PrevPoints), len(out.Flows.Movers))
	}
}

func TestBuildAnalytics_PastWindowReadsAsOfItsLastDay(t *testing.T) {
	q := anaQuery(6)
	q.Preset = "custom"
	q.From, q.To = q.From.AddDate(0, 0, -10), q.To.AddDate(0, 0, -10)
	be := newAnaStub()
	be.snaps = []storageif.AnalysisSnapshotRow{snap("A", 90, 0, 0, 0, map[string]int{})}
	be.hist = &storageif.AnalysisHistoryRows{InWindow: []storageif.AnalysisHistoryRow{{FlowID: "A", Day: dayOf(q, 1), HealthScore: 40, Runs: 1, ByRule: map[string]int{}}}}
	svc := NewDashboardService(be, nil, nil, nil)
	svc.scope = &stubScope{ids: []string{"A"}}

	out, err := svc.BuildAnalytics(context.Background(), "user-1", AnalyticsCaller{}, q)
	if err != nil {
		t.Fatalf("BuildAnalytics: %v", err)
	}
	if out.Summary.AsOf != dayOf(q, 5) {
		t.Errorf("AsOf = %s, want the window's last day %s", out.Summary.AsOf, dayOf(q, 5))
	}
	if out.Summary.AvgHealth.Value != 40 {
		t.Errorf("AvgHealth = %v, want 40 (the state at the end of the window, not today's 90)", out.Summary.AvgHealth.Value)
	}
	if len(out.Trend.Points) != 6 || intp(out.Trend.Points[5].AvgHealth) != "40" {
		t.Errorf("trend = %d points ending %s", len(out.Trend.Points), intp(out.Trend.Points[len(out.Trend.Points)-1].AvgHealth))
	}
}

func TestBuildAnalytics_DerivedFiltersAndFacets(t *testing.T) {
	errRule := pickRule(t, "error", true)
	warnRule := pickRule(t, "warning", false)
	q := anaQuery(7)
	be := newAnaStub()
	a := snap("A", 55, 2, 1, 1, map[string]int{errRule.ID(): 2, warnRule.ID(): 1, "custom-x": 1})
	a.ByConfidence = map[string]int{"high": 3, "low": 1}
	a.AutoFixable = 2
	be.snaps = []storageif.AnalysisSnapshotRow{a}
	svc := NewDashboardService(be, &AnalysisService{}, nil, nil)
	svc.scope = &stubScope{ids: []string{"A"}}
	ctx := context.Background()

	raw, err := svc.BuildAnalytics(ctx, "user-1", AnalyticsCaller{}, q)
	if err != nil {
		t.Fatalf("BuildAnalytics (raw): %v", err)
	}
	if raw.Applied.Derived || raw.Summary.OpenFindings.Value != 4 || raw.Summary.Errors.Value != 2 || raw.Summary.Info.Value != 1 || raw.Summary.AutoFixable.Value != 2 {
		t.Errorf("raw tiles = %+v", raw.Summary)
	}
	if raw.Findings.BySeverity["error"] != 2 || raw.Findings.BySeverity["warning"] != 1 || raw.Findings.BySeverity["info"] != 1 {
		t.Errorf("raw BySeverity = %v", raw.Findings.BySeverity)
	}
	if len(raw.Findings.ByRule) != 3 || raw.Findings.ByRule[0].Rule != errRule.ID() || raw.Findings.ByRule[0].Count != 2 || raw.Findings.ByRule[0].Flows != 1 || raw.Findings.ByRule[0].Severity != "error" || !raw.Findings.ByRule[0].AutoFixable {
		t.Errorf("raw ByRule = %+v", raw.Findings.ByRule)
	}
	var custom *struct{ cat, name string }
	for _, r := range raw.Findings.ByRule {
		if r.Rule == "custom-x" {
			if r.InCatalog {
				t.Error("custom-x must be flagged as outside the catalog")
			}
			custom = &struct{ cat, name string }{r.Category, r.Name}
		}
	}
	if custom == nil || custom.cat != otherCategory || custom.name != "custom-x" {
		t.Errorf("unknown rule → category Other, name = id: %+v", custom)
	}
	foundOther := false
	for _, c := range raw.Findings.ByCategory {
		if c.Category == otherCategory && c.Count == 1 {
			foundOther = true
		}
		if c.Category == errRule.Category() && c.Errors < 2 {
			t.Errorf("category %s errors = %d, want ≥2", c.Category, c.Errors)
		}
	}
	if !foundOther {
		t.Errorf("ByCategory lacks Other(1): %+v", raw.Findings.ByCategory)
	}
	if raw.Findings.ByConfidence["high"] != 3 || raw.Findings.ByConfidence["low"] != 1 {
		t.Errorf("ByConfidence = %v", raw.Findings.ByConfidence)
	}
	if raw.Findings.Fixability.TotalRules != len(analyzer.AllRules()) || raw.Findings.Fixability.Total != 4 || raw.Findings.Fixability.Available != 2 {
		t.Errorf("Fixability = %+v", raw.Findings.Fixability)
	}
	facet := func(list []struct{}, _ string) {}
	_ = facet
	sev := map[string]int{}
	for _, f := range raw.Facets.Severity {
		sev[f.Key] = f.Count
	}
	if sev["error"] != 2 || sev["warning"] != 1 || sev["info"] != 0 {
		t.Errorf("severity facets = %v (custom-x has no severity)", sev)
	}
	if len(raw.Facets.Rule) != 3 || len(raw.Facets.Flows) != 1 || intp(raw.Facets.Flows[0].Health) != "55" || raw.Facets.Flows[0].Findings != 4 {
		t.Errorf("facets = rules %d, flows %+v", len(raw.Facets.Rule), raw.Facets.Flows)
	}
	if raw.Flows.Worst[0].Findings != 4 || len(raw.Flows.Complexity) != 1 || raw.Flows.HealthBuckets[2].Count != 1 {
		t.Errorf("flows section = worst %+v, complexity %d, buckets %+v", raw.Flows.Worst, len(raw.Flows.Complexity), raw.Flows.HealthBuckets)
	}

	q.Severity = []string{"error"}
	der, err := svc.BuildAnalytics(ctx, "user-1", AnalyticsCaller{}, q)
	if err != nil {
		t.Fatalf("BuildAnalytics (severity=error): %v", err)
	}
	if !der.Applied.Derived || der.Summary.OpenFindings.Value != 2 || der.Summary.Errors.Value != 2 || der.Summary.Warnings.Value != 0 || der.Summary.Info.Value != 0 {
		t.Errorf("derived tiles = %+v", der.Summary)
	}
	if der.Findings.BySeverity["warning"] != 0 || len(der.Findings.ByRule) != 1 || der.Findings.ByRule[0].Rule != errRule.ID() {
		t.Errorf("derived findings = %v / %+v", der.Findings.BySeverity, der.Findings.ByRule)
	}
	sev = map[string]int{}
	for _, f := range der.Facets.Severity {
		sev[f.Key] = f.Count
	}
	if sev["error"] != 2 || sev["warning"] != 1 {
		t.Errorf("severity facets must ignore the severity filter itself: %v", sev)
	}
	if len(der.Facets.Rule) != 1 || der.Facets.Rule[0].Key != errRule.ID() {
		t.Errorf("rule facets under severity=error = %+v", der.Facets.Rule)
	}
	for _, c := range der.Facets.Category {
		if c.Key != errRule.Category() {
			t.Errorf("category facet %q must not survive severity=error", c.Key)
		}
	}
	if der.Facets.Flows[0].Findings != 2 || der.Flows.Worst[0].Findings != 2 || der.Flows.Worst[0].Warnings != 0 {
		t.Errorf("per-flow counts must be derived too: facet %d, worst %+v", der.Facets.Flows[0].Findings, der.Flows.Worst[0])
	}

	q.Severity = nil
	q.Category = []string{otherCategory}
	oth, err := svc.BuildAnalytics(ctx, "user-1", AnalyticsCaller{}, q)
	if err != nil {
		t.Fatalf("BuildAnalytics (category=Other): %v", err)
	}
	if oth.Summary.OpenFindings.Value != 1 || len(oth.Findings.ByRule) != 1 || oth.Findings.ByRule[0].Rule != "custom-x" {
		t.Errorf("category=Other = %v findings, rules %+v", oth.Summary.OpenFindings.Value, oth.Findings.ByRule)
	}
}

func TestBuildAnalytics_FlowsFilterIntersectsScope(t *testing.T) {
	q := anaQuery(7)
	q.Flows = []string{"C", "ghost"}
	be := newAnaStub()
	be.snaps = []storageif.AnalysisSnapshotRow{snap("A", 90, 0, 0, 0, map[string]int{}), snap("B", 80, 0, 0, 0, map[string]int{}), snap("C", 70, 0, 0, 0, map[string]int{})}
	svc := NewDashboardService(be, nil, nil, nil)
	svc.scope = &stubScope{ids: []string{"A", "B", "C"}}

	out, err := svc.BuildAnalytics(context.Background(), "user-1", AnalyticsCaller{}, q)
	if err != nil {
		t.Fatalf("BuildAnalytics: %v", err)
	}
	if strings.Join(out.Applied.Flows, ",") != "C" {
		t.Errorf("applied flows = %v, want only the visible C (ghost dropped)", out.Applied.Flows)
	}
	if out.Summary.FlowsInScope != 1 || out.Summary.AvgHealth.Value != 70 {
		t.Errorf("scope = %d flows, avg %v; want 1 / 70", out.Summary.FlowsInScope, out.Summary.AvgHealth.Value)
	}
	if len(out.Facets.Flows) != 3 {
		t.Errorf("the flow picker lists the whole scope: %d", len(out.Facets.Flows))
	}
	sizes := map[int]bool{}
	for _, c := range be.snapCalls {
		sizes[len(c)] = true
	}
	if !sizes[1] || !sizes[3] {
		t.Errorf("snapshot reads = %v, want one for the filtered set and one for the scope", be.snapCalls)
	}
}

func TestBuildAnalytics_ScopeErrorPropagatesUncached(t *testing.T) {
	q := anaQuery(30)
	q.Scope, q.OrgID = "org", "acme"
	scope := &stubScope{err: ErrPermissionDenied}
	svc := NewDashboardService(newAnaStub(), nil, nil, nil)
	svc.scope = scope

	for i := 0; i < 2; i++ {
		_, err := svc.BuildAnalytics(context.Background(), "user-1", AnalyticsCaller{}, q)
		if !errors.Is(err, ErrPermissionDenied) {
			t.Fatalf("call %d: err = %v, want ErrPermissionDenied", i, err)
		}
	}
	if scope.count() != 2 {
		t.Errorf("scope resolved %d times, want 2 (a denial is never cached)", scope.count())
	}
	if scope.lastOrg != "acme" || scope.lastScope != ScopeAll {
		t.Errorf("org scope resolved as %q/%q", scope.lastOrg, scope.lastScope)
	}
}

func TestBuildAnalytics_PartialSections(t *testing.T) {
	q := anaQuery(7)
	be := newAnaStub()
	be.fail["trend"] = errors.New("history timed out")
	be.fail["ai"] = errors.New("usage timed out")
	svc := NewDashboardService(be, nil, nil, nil)
	svc.scope = &stubScope{ids: []string{"A"}}

	out, err := svc.BuildAnalytics(context.Background(), "user-1", AnalyticsCaller{}, q)
	if err != nil {
		t.Fatalf("a failed section must not fail the request: %v", err)
	}
	if strings.Join(out.Partial, ",") != "trend,ai" {
		t.Errorf("Partial = %v, want [trend ai]", out.Partial)
	}
	if out.Trend.Points == nil || len(out.Trend.Points) != 0 || out.Summary.AnalysesRun.Available || len(out.AI.Daily) != 0 {
		t.Errorf("failed sections must be empty and unavailable: points=%v runs=%+v ai=%+v", out.Trend.Points, out.Summary.AnalysesRun, out.AI)
	}
}

func TestBuildAnalytics_CacheAndInvalidation(t *testing.T) {
	q := anaQuery(30)
	scope := &stubScope{ids: []string{"A"}}
	svc := NewDashboardService(newAnaStub(), nil, nil, nil)
	svc.scope = scope
	ctx := context.Background()
	call := func(user string, q AnalyticsQuery) {
		t.Helper()
		if _, err := svc.BuildAnalytics(ctx, user, AnalyticsCaller{}, q); err != nil {
			t.Fatalf("BuildAnalytics: %v", err)
		}
	}
	call("user-1", q)
	call("user-1", q)
	if scope.count() != 1 {
		t.Fatalf("second identical call must hit the cache: %d resolutions", scope.count())
	}
	q2 := q
	q2.Compare = false
	call("user-1", q2)
	if scope.count() != 2 {
		t.Fatalf("a different query is a different entry: %d", scope.count())
	}
	svc.InvalidateDashboardCache("user-1")
	call("user-1", q)
	if scope.count() != 3 {
		t.Fatalf("invalidating the user drops every analytics view they built: %d", scope.count())
	}
	call("user-2", q)
	svc.InvalidateDashboardCache("user-1")
	call("user-2", q)
	if scope.count() != 4 {
		t.Fatalf("another user's entries survive: %d", scope.count())
	}
	svc.InvalidateDashboardCache("")
	call("user-2", q)
	if scope.count() != 5 {
		t.Fatalf("a global invalidation drops everything: %d", scope.count())
	}

	// Concurrent cold calls collapse into one build.
	svc.InvalidateDashboardCache("")
	before := scope.count()
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			call("user-3", q)
		}()
	}
	wg.Wait()
	if scope.count() != before+1 {
		t.Errorf("20 concurrent cold calls resolved the scope %d times, want 1", scope.count()-before)
	}
}

func TestBuildAnalytics_SectionsAndScoping(t *testing.T) {
	errRule := pickRule(t, "error", false)
	q := anaQuery(7)
	be := newAnaStub()
	be.snaps = []storageif.AnalysisSnapshotRow{snap("A", 70, 1, 0, 0, map[string]int{errRule.ID(): 1})}
	prevDay := q.PrevFrom().Format(analyticsDay)
	be.triage = &storageif.TriageRows{
		ByStatusRule:  []storageif.TriageStatusRule{{Status: "open", RuleID: errRule.ID(), Count: 3}, {Status: "resolved", RuleID: errRule.ID(), Count: 2}, {Status: "suppressed", RuleID: "other", Count: 1}},
		ResolvedByDay: []storageif.TriageResolvedDay{{Date: dayOf(q, 2), RuleID: errRule.ID(), Count: 2}, {Date: prevDay, RuleID: errRule.ID(), Count: 1}},
		MttrHours:     5, ResolvedCount: 2, StaleCount: 1,
	}
	be.usage = &storageif.UsageRows{
		Daily:      []storageif.UsageDailyRow{{Date: dayOf(q, 0), TokensIn: 100, TokensOut: 50, Calls: 2}, {Date: prevDay, TokensIn: 10, TokensOut: 5, Calls: 1}},
		ByProvider: []storageif.ProviderUsage{{Provider: "openai", TokensIn: 100, TokensOut: 50}},
		ByModel:    []storageif.UsageModelRow{{Provider: "openai", Model: "gpt", ServedModel: "gpt-4o-2024-11-20", TokensIn: 100, TokensOut: 50, Calls: 2}},
	}
	be.activity = &storageif.ActivityRows{
		Events:   []storageif.ActivityEventRow{{Action: "flow.upload", UserID: "user-1", ActorName: "Me", ResourceType: "flow", ResourceID: "A", Label: "Flow A", CreatedAt: time.Now()}, {Action: "flow.share", UserID: "user-9", ActorName: "Ada", ResourceType: "flow", ResourceID: "A", Label: "Flow A", CreatedAt: time.Now()}},
		ByDay:    []storageif.DateCountRow{{Date: dayOf(q, 1), Count: 4}},
		ByAction: []storageif.ActionCountRow{{Action: "flow.upload", Count: 4}},
	}
	be.security = &storageif.SecurityRows{FailedLogins24h: 2, LockedAccounts: 3, LockedRead: true}
	svc := NewDashboardService(be, &AnalysisService{}, nil, nil)
	svc.scope = &stubScope{ids: []string{"A"}}
	ctx := context.Background()

	out, err := svc.BuildAnalytics(ctx, "user-1", AnalyticsCaller{Email: "me@example.com"}, q)
	if err != nil {
		t.Fatalf("BuildAnalytics: %v", err)
	}
	tr := out.Triage
	if !tr.Available || tr.Funnel["open"] != 3 || tr.Funnel["resolved"] != 2 || tr.Funnel["suppressed"] != 1 || tr.Backlog != 3 || tr.MttrHours != 5 || tr.StaleCount != 1 {
		t.Errorf("triage = %+v", tr)
	}
	if len(tr.ResolvedByDay) != 7 || tr.ResolvedByDay[2].Count != 2 || tr.ResolvedByDay[0].Count != 0 {
		t.Errorf("resolved per day must be gap-filled over the window: %+v", tr.ResolvedByDay)
	}
	if out.Summary.Resolved.Value != 2 || out.Summary.Resolved.Prev == nil || *out.Summary.Resolved.Prev != 1 {
		t.Errorf("Resolved tile = %+v, want 2 vs 1", out.Summary.Resolved)
	}
	ai := out.AI
	if ai.Scoped != "user" || ai.FlowScoped || ai.TotalTokens != 150 || len(ai.Daily) != 7 || len(ai.ByProvider) != 1 || len(ai.ByModel) != 1 {
		t.Errorf("ai = %+v", ai)
	}
	if len(ai.ByModel) == 1 && (ai.ByModel[0].Model != "gpt" || ai.ByModel[0].ServedModel != "gpt-4o-2024-11-20") {
		t.Errorf("ByModel row must carry the served model next to the requested one (D-42): %+v", ai.ByModel[0])
	}
	act := out.Activity
	if len(act.Events) != 2 || !act.Events[0].Actor.IsMe || act.Events[1].Actor.IsMe || act.Events[1].Actor.Name != "Ada" || act.Events[0].Label != "Flow A" {
		t.Errorf("activity events = %+v", act.Events)
	}
	if len(act.ByDay) != 7 || act.ByDay[1].Count != 4 || len(act.ByAction) != 1 {
		t.Errorf("activity series = %+v / %+v", act.ByDay, act.ByAction)
	}
	sec := out.Security
	if !sec.Available || sec.FailedLogins24h != 2 || sec.LockedAccounts != nil {
		t.Errorf("member security = %+v, want failed logins 2 and no locked-account count", sec)
	}
	if sec.SecurityFindings != 0 && !strings.EqualFold(errRule.Category(), "security") {
		t.Errorf("security findings = %d for a %s rule", sec.SecurityFindings, errRule.Category())
	}

	admin, err := svc.BuildAnalytics(ctx, "admin-1", AnalyticsCaller{IsAdmin: true}, q)
	if err != nil {
		t.Fatalf("BuildAnalytics (admin): %v", err)
	}
	if intp(admin.Security.LockedAccounts) != "3" {
		t.Errorf("admin locked accounts = %s, want 3", intp(admin.Security.LockedAccounts))
	}

	// Usage scoping follows the flow scope: personal → the caller's calls,
	// org → that org's, all with orgs → both.
	scoped := &stubScope{ids: []string{"A"}, orgIDs: []string{"org-1"}}
	svc.scope = scoped
	svc.InvalidateDashboardCache("")
	qo := q
	qo.Scope, qo.OrgID = "org", "org-1"
	if o, err := svc.BuildAnalytics(ctx, "user-1", AnalyticsCaller{}, qo); err != nil || o.AI.Scoped != "org" {
		t.Errorf("org scope → ai scoped %q (err %v), want org", o.AI.Scoped, err)
	}
	qa := q
	qa.Scope = "all"
	if o, err := svc.BuildAnalytics(ctx, "user-1", AnalyticsCaller{}, qa); err != nil || o.AI.Scoped != "user+orgs" {
		t.Errorf("all scope with orgs → ai scoped %q (err %v), want user+orgs", o.AI.Scoped, err)
	}
	qp := q
	qp.Scope = "personal"
	if o, err := svc.BuildAnalytics(ctx, "user-1", AnalyticsCaller{}, qp); err != nil || o.AI.Scoped != "user" {
		t.Errorf("personal scope → ai scoped %q (err %v), want user", o.AI.Scoped, err)
	}
	if scoped.lastScope != ScopeMine {
		t.Errorf("personal scope must resolve as ScopeMine, got %q", scoped.lastScope)
	}
	modes := strings.Join(be.usageModes, ",")
	if !strings.Contains(modes, "org") || !strings.Contains(modes, "both") || !strings.Contains(modes, "user") {
		t.Errorf("usage read modes = %v", be.usageModes)
	}
}

func TestBuildAnalytics_EmptyIsSerialisable(t *testing.T) {
	q := anaQuery(7)
	svc := NewDashboardService(newAnaStub(), nil, nil, nil)
	svc.scope = &stubScope{}

	out, err := svc.BuildAnalytics(context.Background(), "user-1", AnalyticsCaller{}, q)
	if err != nil {
		t.Fatalf("BuildAnalytics: %v", err)
	}
	if out.Summary.AvgHealth.Available || out.Summary.FlowsInScope != 0 || out.Triage.Available {
		t.Errorf("empty scope must read as unavailable: %+v", out.Summary)
	}
	raw, err := json.Marshal(out)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	js := string(raw)
	// The date axis is always gap-filled — seven days of "no data yet" (null
	// health, never 0) — while every other collection is an empty list.
	if len(out.Trend.Points) != 7 || len(out.Trend.PrevPoints) != 7 || out.Trend.Points[6].AvgHealth != nil {
		t.Errorf("empty trend = %d/%d points, last health %s", len(out.Trend.Points), len(out.Trend.PrevPoints), intp(out.Trend.Points[len(out.Trend.Points)-1].AvgHealth))
	}
	for _, want := range []string{`"avgHealth":null`, `"byRule":[]`, `"byCategory":[]`, `"partial":[]`, `"events":[]`, `"flows":[]`, `"rule":[]`, `"healthBuckets":[`, `"funnel":{}`, `"lockedAccounts":null`} {
		if !strings.Contains(js, want) {
			t.Errorf("payload lacks %s: %s", want, js)
		}
	}
	if strings.Contains(js, `:null,`) && strings.Count(js, `:null`) != strings.Count(js, `"avgHealth":null`)+strings.Count(js, `"prev":null`)+strings.Count(js, `"lockedAccounts":null`) {
		t.Errorf("only avgHealth, prev and lockedAccounts may be null: %s", js)
	}
}

func TestBuildAnalytics_NeedsCloudBackendAndResolver(t *testing.T) {
	q := anaQuery(7)
	local := NewDashboardService(nil, nil, nil, nil)
	if local.CloudMode() {
		t.Error("nil backend is local mode")
	}
	if _, err := local.BuildAnalytics(context.Background(), "u", AnalyticsCaller{}, q); err == nil {
		t.Error("local mode must refuse analytics")
	}
	cloud := NewDashboardService(newAnaStub(), nil, nil, nil)
	if !cloud.CloudMode() {
		t.Error("a backend means cloud mode")
	}
	if _, err := cloud.BuildAnalytics(context.Background(), "u", AnalyticsCaller{}, q); err == nil {
		t.Error("a missing scope resolver must be an error, not a panic")
	}
}

// TestAnaCacheFill_BoundedByEntryCap pins the cache bound: the analytics key
// embeds caller-chosen From/To nanoseconds and flow lists, so distinct keys
// are unbounded input. Inserting well over anaCacheMaxEntries entries must
// keep the map at ≤ cap (expired-first sweep, then nearest-to-expiry
// eviction), and the most recently inserted key must still be resident.
func TestAnaCacheFill_BoundedByEntryCap(t *testing.T) {
	svc := NewDashboardService(newAnaStub(), nil, nil, nil)

	const extra = 50
	for i := 0; i < anaCacheMaxEntries+extra; i++ {
		svc.anaCacheFill(context.Background(), fmt.Sprintf("user-1\x00q%d", i), &models.DashboardAnalyticsData{}, svc.beginAnaFill("user-1"))
	}

	svc.ana.mu.Lock()
	size := len(svc.ana.m)
	_, newestResident := svc.ana.m[fmt.Sprintf("user-1\x00q%d", anaCacheMaxEntries+extra-1)]
	_, oldestResident := svc.ana.m["user-1\x00q0"]
	svc.ana.mu.Unlock()

	if size > anaCacheMaxEntries {
		t.Errorf("cache size = %d after %d distinct inserts, want ≤ %d", size, anaCacheMaxEntries+extra, anaCacheMaxEntries)
	}
	if !newestResident {
		t.Error("the most recently inserted entry must survive eviction")
	}
	if oldestResident {
		t.Error("the oldest entry should have been evicted once the cap was exceeded")
	}
}
