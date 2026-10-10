package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/events"
	"pad-analyzer/internal/metrics"
	"pad-analyzer/internal/testutil"
	"pad-core/analyzer"
	"pad-core/cache"
	"pad-core/models"
	"pad-core/parser"
)

const tenantTestSrc = "#Region \"Main\"\n    HTTPClient.InvokeUrl Url: $'''https://a''' Method: HTTPClient.Method.GET\n#EndRegion\n"

func callerCtx(userID string) context.Context {
	return auth.WithClaims(context.Background(), &auth.Claims{UserID: userID})
}

func newCloudAnalysisService(t *testing.T) *AnalysisService {
	t.Helper()
	svc, err := NewAnalysisService(NilNotifier{}, nilSettings{}, nil, AnalysisOptions{AuthEnabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// cacheTenant is whose report a document's is: the local user without accounts, else the stored flow's
// organisation or owner, else the caller; nobody is the zero part (analyzed, never cached).
func TestCacheTenant(t *testing.T) {
	desktop := newDesktopAnalysisService(t)
	cloud := newCloudAnalysisService(t)
	orgFlow := &models.FlowDocument{ID: "f1", OwnerID: "u1", OrganizationID: "o1"}
	personal := &models.FlowDocument{ID: "f2", OwnerID: "u1"}
	raw := &models.FlowDocument{ID: "parse-uuid", StableID: "files-abc"}

	for _, c := range []struct {
		name string
		svc  *AnalysisService
		ctx  context.Context
		doc  *models.FlowDocument
		want cache.Part
	}{
		{"desktop", desktop, context.Background(), raw, cache.Local()},
		{"desktop, even with claims", desktop, callerCtx("u9"), orgFlow, cache.Local()},
		{"org flow", cloud, callerCtx("u9"), orgFlow, cache.Org("o1")},
		{"personal flow", cloud, callerCtx("u9"), personal, cache.User("u1")},
		{"stored flow, no caller (scanner, CI webhook)", cloud, context.Background(), orgFlow, cache.Org("o1")},
		{"path-less input: the caller", cloud, callerCtx("u9"), raw, cache.User("u9")},
		{"path-less input, no caller", cloud, context.Background(), raw, cache.Part{}},
	} {
		if got := c.svc.cacheTenant(c.ctx, c.doc); got != c.want {
			t.Errorf("%s: cacheTenant = %v, want %v", c.name, got, c.want)
		}
	}
}

// A stored flow has one entry for everyone who may read it; two callers posting the same path-less files get their
// own report each, with their own flow name and block ids (B1/B2 through the service).
func TestAnalyzeFlow_TenantScopedCache(t *testing.T) {
	svc := newCloudAnalysisService(t)
	stored := makeAnalyzableDoc("flow-1",
		models.Block{ID: "b1", SubflowID: "main", Name: "Set X", Type: models.BlockTypeAction, RawType: "SetVariable.Set", LineNumber: 1})
	stored.OwnerID, stored.OrganizationID = "u1", "o1"

	r1, err := svc.AnalyzeFlow(callerCtx("u1"), stored)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := svc.AnalyzeFlow(callerCtx("u2"), stored)
	if err != nil {
		t.Fatal(err)
	}
	if r1 != r2 {
		t.Error("two readers of one org flow did not share its cached report")
	}
	if got := svc.analyzer.Reports(cache.Org("o1")); len(got) != 1 || got[0] != r1 {
		t.Errorf("the org's entries: %d, want the flow's one", len(got))
	}

	parse := func(root string) *models.FlowDocument {
		doc, err := parser.ParseFiles(map[string]string{"Main.txt": tenantTestSrc}, root)
		if err != nil {
			t.Fatal(err)
		}
		return doc
	}
	victim, attacker := parse("victim-project"), parse("attacker")
	rv, err := svc.AnalyzeRaw(callerCtx("victim"), victim)
	if err != nil {
		t.Fatal(err)
	}
	ra, err := svc.AnalyzeRaw(callerCtx("attacker"), attacker)
	if err != nil {
		t.Fatal(err)
	}
	if ra == rv || ra.FlowName != "attacker" || ra.FlowID != attacker.ID {
		t.Fatalf("the second caller got flow %q (%s), want its own %q (%s)", ra.FlowName, ra.FlowID, "attacker", attacker.ID)
	}
	for _, f := range ra.Findings {
		if f.BlockID != "" && attacker.BlocksByID[f.BlockID] == nil {
			t.Errorf("finding %s names block %s, which the caller's document does not have", f.ID, f.BlockID)
		}
	}
}

// analyze-raw is no stored flow: it records no diff pair (cache survey Q2), while AnalyzeFlow does.
func TestAnalyzeRaw_RecordsNothing(t *testing.T) {
	svc := newCloudAnalysisService(t)
	doc, err := parser.ParseFiles(map[string]string{"Main.txt": tenantTestSrc}, "Main")
	if err != nil {
		t.Fatal(err)
	}
	if r, err := svc.AnalyzeRaw(callerCtx("u1"), doc); err != nil || r == nil {
		t.Fatalf("AnalyzeRaw: %v", err)
	}
	if _, ok := svc.CurrentReport(doc); ok {
		t.Error("analyze-raw recorded the flow's current report")
	}
	if _, err := svc.AnalyzeFlow(callerCtx("u1"), doc); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.CurrentReport(doc); !ok {
		t.Error("control: AnalyzeFlow did not record")
	}
}

// With nobody to scope to, the analysis runs without the cache.
func TestAnalyzeFlow_NoTenantIsNotCached(t *testing.T) {
	svc := newCloudAnalysisService(t)
	doc, err := parser.ParseFiles(map[string]string{"Main.txt": tenantTestSrc}, "Main")
	if err != nil {
		t.Fatal(err)
	}
	r1, err := svc.AnalyzeFlowReadOnly(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	r2, err := svc.AnalyzeFlowReadOnly(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	if r1 == r2 {
		t.Error("an analysis without a tenant was served from the cache")
	}
}

// The desktop session analytics live in the analyzer the flow service shares: opening a folder resets them, a
// reimport drops the reimported flow, and a deleted flow (FlowChanged) drops its report.
func TestSessionAnalytics_SharedWithTheFlowService(t *testing.T) {
	analysis := newDesktopAnalysisService(t)
	flows := NewFlowService(&testutil.CountingNotifier{}, newTestSettingsStore(t), NewLocalDocumentProvider(), nil, nil, nil,
		WithAnalyzer(analysis.analyzer))
	dashboard := NewDashboardService(nil, analysis, flows, nil)
	bus := subscribedBus(flows, dashboard, &ChatService{chatCtxCache: newChatContextCache()})
	flows.bus = bus
	ctx := context.Background()

	dir := t.TempDir()
	load := func(name string) *models.FlowDocument {
		t.Helper()
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte(tenantTestSrc), 0o600); err != nil {
			t.Fatal(err)
		}
		doc, err := flows.LoadFlowFromPath(path)
		if err != nil {
			t.Fatalf("load %s: %v", name, err)
		}
		if _, err := analysis.AnalyzeFlow(ctx, doc); err != nil {
			t.Fatal(err)
		}
		return doc
	}
	flowsAnalyzed := func() int { return analysis.ComputeDashboard().TotalFlowsAnalyzed }

	a, b := load("a.txt"), load("b.txt")
	if n := flowsAnalyzed(); n != 2 {
		t.Fatalf("session analytics count %d flows, want 2", n)
	}
	if home := dashboard.BuildHome(ctx, ""); home.Overview.TotalFlows != 2 {
		t.Fatalf("desktop home counts %d flows, want 2", home.Overview.TotalFlows)
	}

	if _, err := flows.ReimportFlow(ctx, a); err != nil {
		t.Fatalf("reimport: %v", err)
	}
	if n := flowsAnalyzed(); n != 1 {
		t.Errorf("after reimporting a: %d flows, want 1 (b)", n)
	}

	bus.Publish(ctx, events.FlowChanged{FlowID: b.ID, StableID: analyzer.StableFlowID(b), Kind: events.FlowDeleted})
	if n := flowsAnalyzed(); n != 0 {
		t.Errorf("after deleting b: %d flows, want 0", n)
	}

	load("c.txt")
	load("d.txt")
	flows.ResetSessionAnalytics()
	if n := flowsAnalyzed(); n != 0 {
		t.Errorf("after the folder reset: %d flows, want 0", n)
	}
	if home := dashboard.BuildHome(ctx, ""); home.Overview.TotalFlows != 0 {
		t.Errorf("desktop home after the reset counts %d flows, want 0", home.Overview.TotalFlows)
	}
}

// With accounts, analyze-raw bypasses the report cache: each post is a fresh parse no later request hits, so storing
// it under the caller only evicted stored flows' reports. Without accounts its report still joins the session
// analytics.
func TestAnalyzeRaw_WithAccountsSkipsTheCache(t *testing.T) {
	svc, err := NewAnalysisService(NilNotifier{}, nilSettings{}, nil, AnalysisOptions{AuthEnabled: true,
		Analyzer: analyzer.NewCachedAnalyzer(analyzer.WithReportCache(2, 0))})
	if err != nil {
		t.Fatal(err)
	}
	stored := makeAnalyzableDoc("flow-1",
		models.Block{ID: "b1", SubflowID: "main", Name: "Set X", Type: models.BlockTypeAction, RawType: "SetVariable.Set", LineNumber: 1})
	stored.OwnerID, stored.OrganizationID = "u1", "o1"
	orgReport, err := svc.AnalyzeFlow(callerCtx("u1"), stored)
	if err != nil {
		t.Fatal(err)
	}

	callers := []string{"ci-0", "ci-1", "ci-2"}
	for _, c := range callers {
		doc, err := parser.ParseFiles(map[string]string{"Main.txt": tenantTestSrc}, "Main")
		if err != nil {
			t.Fatal(err)
		}
		if r, err := svc.AnalyzeRaw(callerCtx(c), doc); err != nil || r == nil {
			t.Fatalf("AnalyzeRaw(%s): %v", c, err)
		}
	}
	for _, c := range callers {
		if n := len(svc.analyzer.Reports(cache.User(c))); n != 0 {
			t.Errorf("%s: %d analyze-raw report(s) stored", c, n)
		}
	}
	if got := svc.analyzer.Reports(cache.Org("o1")); len(got) != 1 || got[0] != orgReport {
		t.Error("the stored org flow's report was evicted by analyze-raw posts")
	}

	desktop := newDesktopAnalysisService(t)
	doc, err := parser.ParseFiles(map[string]string{"Main.txt": tenantTestSrc}, "Main")
	if err != nil {
		t.Fatal(err)
	}
	r, err := desktop.AnalyzeRaw(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	if got := desktop.analyzer.Reports(cache.Local()); len(got) != 1 || got[0] != r {
		t.Errorf("without accounts: %d session report(s), want the analyze-raw report", len(got))
	}
}

// analysisRuns reads pad_analysis_runs_total through the real registry.
func analysisRuns(t *testing.T) float64 {
	t.Helper()
	fams, err := metrics.Gather()
	if err != nil {
		t.Fatalf("gather: %v", err)
	}
	for _, f := range fams {
		if f.GetName() == "pad_analysis_runs_total" {
			return f.GetMetric()[0].GetCounter().GetValue()
		}
	}
	return 0
}

// analyze-raw records nothing but is still an analysis run; an internal read-only analysis is not.
func TestAnalyzeRaw_CountsAsAnAnalysisRun(t *testing.T) {
	for _, svc := range []*AnalysisService{newCloudAnalysisService(t), newDesktopAnalysisService(t)} {
		doc, err := parser.ParseFiles(map[string]string{"Main.txt": tenantTestSrc}, "Main")
		if err != nil {
			t.Fatal(err)
		}
		before := analysisRuns(t)
		if _, err := svc.AnalyzeRaw(callerCtx("ci"), doc); err != nil {
			t.Fatal(err)
		}
		if d := analysisRuns(t) - before; d != 1 {
			t.Errorf("auth %v: analyze-raw counted %v runs, want 1", svc.authEnabled, d)
		}
		before = analysisRuns(t)
		if _, err := svc.AnalyzeFlowReadOnly(callerCtx("ci"), doc); err != nil {
			t.Fatal(err)
		}
		if d := analysisRuns(t) - before; d != 0 {
			t.Errorf("auth %v: a read-only analysis counted %v runs, want 0", svc.authEnabled, d)
		}
	}
}

// The desktop's folder batch analyzes as the local user: its reports join the session analytics, and opening one
// of the flows afterwards is served the batch's report.
func TestAnalyzeBatch_JoinsTheSessionAnalytics(t *testing.T) {
	svc := newDesktopAnalysisService(t)
	dir := t.TempDir()
	a := desktopDoc("a", filepath.Join(dir, "a.txt"), "Set A")
	b := desktopDoc("b", filepath.Join(dir, "b.txt"), "Set B")
	batch, err := svc.AnalyzeBatch([]*models.FlowDocument{a, b})
	if err != nil || batch == nil || len(batch.Results) != 2 {
		t.Fatalf("AnalyzeBatch: %v", err)
	}
	if n := svc.ComputeDashboard().TotalFlowsAnalyzed; n != 2 {
		t.Errorf("session analytics count %d flows after the batch, want 2", n)
	}
	r, err := svc.AnalyzeFlow(context.Background(), a)
	if err != nil {
		t.Fatal(err)
	}
	if r != batch.Results[0].Report {
		t.Error("opening a batch-analyzed flow re-analyzed it instead of hitting the batch's report")
	}
}
