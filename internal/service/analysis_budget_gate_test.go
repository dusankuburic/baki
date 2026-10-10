package service

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/sync/semaphore"

	"pad-core/analyzer"
	"pad-core/models"
)

// Tests for the 2026-10 performance round 4 (analysis service side): the byte-weighted
// report pair cache, the large-analysis gate and the background insertion ceiling.

// findingsReport is a report with n (empty) findings: ReportCost is the 4 KiB base plus n KiB.
func findingsReport(id string, n int) *models.AnalysisReport {
	return &models.AnalysisReport{FlowID: id, Findings: make([]models.Finding, n)}
}

// flatDoc is a document with n top-level action blocks under one subflow, keyed for
// the history pair by its FilePath (two docs with different IDs but the same key are
// "the same flow, edited").
func flatDoc(id, key string, n int) *models.FlowDocument {
	blocks := make([]models.Block, n)
	for i := range blocks {
		blocks[i] = models.Block{ID: fmt.Sprintf("%s-b%d", id, i), Type: models.BlockTypeAction, RawType: "Display.ShowMessage", Name: "Show " + id, SubflowID: "main", LineNumber: i + 1}
	}
	doc := makeAnalyzableDoc(id, blocks...)
	doc.FilePath = `C:\flows\round4-` + key + `.txt`
	return doc
}

// fakeRuns stands in for the analyzer: one fixed report per document ID (a stable
// pointer, like a cache hit would), with the configured finding count; it never walks.
type fakeRuns struct {
	mu      sync.Mutex
	reports map[string]*models.AnalysisReport
	sizes   map[string]int
}

func newFakeRuns() *fakeRuns {
	return &fakeRuns{reports: map[string]*models.AnalysisReport{}, sizes: map[string]int{}}
}

func (f *fakeRuns) size(docID string, findings int) { f.sizes[docID] = findings }

func (f *fakeRuns) run(_ context.Context, in analyzer.Input) *models.AnalysisReport {
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.reports[in.Doc.ID]
	if !ok {
		r = findingsReport(in.Doc.ID, f.sizes[in.Doc.ID])
		f.reports[in.Doc.ID] = r
	}
	return r
}

// newRound4Service builds a service with its own pair-cache budget and the fake
// analyzer, on its own empty analyzer cache (the probe before the gate always misses,
// every call reaches the fake).
func newRound4Service(t *testing.T, budget int64, runs *fakeRuns) *AnalysisService {
	t.Helper()
	svc, err := NewAnalysisService(NilNotifier{}, nilSettings{}, nil, AnalysisOptions{})
	if err != nil {
		t.Fatalf("NewAnalysisService: %v", err)
	}
	svc.reports = newReportLRU(budget)
	if runs != nil {
		svc.run = runs.run
	}
	return svc
}

// TestReportLRU_EvictsByBytesLeastRecentlyUsed: the pair cache is charged by
// ReportCost, not entry count; past the budget the least recently used pairs go
// first, a re-put re-costs the pair, and evictions refund their bytes.
func TestReportLRU_EvictsByBytesLeastRecentlyUsed(t *testing.T) {
	unit := analyzer.ReportCost(findingsReport("", 100))
	c := newReportLRU(3 * unit)
	for _, id := range []string{"a", "b", "c"} {
		if !c.Put(id, &reportPair{current: findingsReport(id, 100)}) {
			t.Fatalf("pair %s not retained", id)
		}
	}
	if n, b := c.Len(), c.Bytes(); n != 3 || b != 3*unit {
		t.Fatalf("cache holds %d pairs / %d bytes, want 3 / %d", n, b, 3*unit)
	}
	if _, ok := c.Get("a"); !ok {
		t.Fatal("a missing before any eviction")
	}

	c.Put("d", &reportPair{current: findingsReport("d", 100)})
	if _, ok := c.Get("b"); ok {
		t.Error("least recently used pair b survived the budget; want it evicted")
	}
	for _, id := range []string{"a", "c", "d"} {
		if _, ok := c.Get(id); !ok {
			t.Errorf("pair %s evicted; want it kept", id)
		}
	}
	if n, b := c.Len(), c.Bytes(); n != 3 || b != 3*unit {
		t.Errorf("cache holds %d pairs / %d bytes, want 3 / %d", n, b, 3*unit)
	}

	e := &reportPair{current: findingsReport("e2", 100), prev: findingsReport("e1", 100)}
	c.Put("e", e)
	if _, ok := c.Get("a"); ok {
		t.Error("a should have been evicted for e")
	}
	if _, ok := c.Get("c"); ok {
		t.Error("c should have been evicted for e")
	}
	if got, ok := c.Get("e"); !ok || got.prev == nil {
		t.Error("e must be retained with its prev (both fit)")
	}
	if n, b := c.Len(), c.Bytes(); n != 2 || b != 3*unit {
		t.Errorf("cache holds %d pairs / %d bytes, want 2 / %d", n, b, 3*unit)
	}

	c.Put("d", &reportPair{current: findingsReport("d2", 100), prev: findingsReport("d", 100)})
	if n, b := c.Len(), c.Bytes(); n != 1 || b != 2*unit {
		t.Errorf("after re-putting d with a prev: %d pairs / %d bytes, want 1 (e evicted) / %d", n, b, 2*unit)
	}
}

// TestReportLRU_DropsPrevFirstAndRejectsAnOversizedCurrent: a pair whose two reports
// exceed the budget keeps only its current; an oversized current is not retained.
func TestReportLRU_DropsPrevFirstAndRejectsAnOversizedCurrent(t *testing.T) {
	budget := analyzer.ReportCost(findingsReport("", 100)) + analyzer.ReportCost(findingsReport("", 50))
	c := newReportLRU(budget)

	pair := &reportPair{current: findingsReport("c", 100), prev: findingsReport("p", 100)}
	if !c.Put("x", pair) {
		t.Fatal("a pair whose current fits must be retained")
	}
	got, ok := c.Get("x")
	if !ok || got.current == nil {
		t.Fatal("x missing")
	}
	if got.prev != nil {
		t.Error("prev should have been dropped first: current + prev exceed the budget")
	}
	if b := c.Bytes(); b != analyzer.ReportCost(got.current) {
		t.Errorf("bytes = %d, want the current report only (%d)", b, analyzer.ReportCost(got.current))
	}

	c.Put("x", &reportPair{current: findingsReport("c2", 50), prev: findingsReport("p2", 100)})
	if got, _ := c.Get("x"); got.prev == nil {
		t.Error("both reports fit: prev must be kept")
	}

	if c.Put("x", &reportPair{current: findingsReport("huge", 200)}) {
		t.Error("a current report larger than the budget was retained")
	}
	if _, ok := c.Get("x"); ok {
		t.Error("the previous pair under the key must not survive an unretained put")
	}
	if n, b := c.Len(), c.Bytes(); n != 0 || b != 0 {
		t.Errorf("cache holds %d pairs / %d bytes, want 0 / 0", n, b)
	}
}

// TestAnalyzeFlow_PairBudgetEvictsTheOldestFlows drives the budget through the
// service: with room for three 100-finding reports, the fourth flow evicts the least
// recently used one, a just-read flow survives, and the diff pair keeps prev.
func TestAnalyzeFlow_PairBudgetEvictsTheOldestFlows(t *testing.T) {
	runs := newFakeRuns()
	unit := analyzer.ReportCost(findingsReport("", 100))
	svc := newRound4Service(t, 3*unit, runs)
	ctx := context.Background()

	docs := map[string]*models.FlowDocument{}
	for _, id := range []string{"a", "b", "c", "d"} {
		runs.size(id, 100)
		docs[id] = flatDoc(id, id, 1)
	}
	for _, id := range []string{"a", "b", "c"} {
		if _, err := svc.AnalyzeFlow(ctx, docs[id]); err != nil {
			t.Fatalf("analyze %s: %v", id, err)
		}
	}
	if _, ok := svc.CurrentReport(docs["a"]); !ok {
		t.Fatal("a missing before any eviction")
	}
	if _, err := svc.AnalyzeFlow(ctx, docs["d"]); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.CurrentReport(docs["b"]); ok {
		t.Error("b (least recently used) should have been evicted by d")
	}
	for _, id := range []string{"a", "c", "d"} {
		if r, ok := svc.CurrentReport(docs[id]); !ok || r != runs.reports[id] {
			t.Errorf("CurrentReport(%s) = (%p, %v), want the flow's report %p", id, r, ok, runs.reports[id])
		}
	}
	if b := svc.reports.Bytes(); b != 3*unit {
		t.Errorf("pair cache charges %d bytes, want %d", b, 3*unit)
	}

	if _, fresh, err := svc.AnalyzeFlowFresh(ctx, docs["b"]); err != nil || !fresh {
		t.Errorf("AnalyzeFlowFresh of the evicted b = (fresh=%v, %v), want fresh", fresh, err)
	}

	runs.size("d2", 100)
	d2 := flatDoc("d2", "d", 1)
	if _, err := svc.AnalyzeFlow(ctx, d2); err != nil {
		t.Fatal(err)
	}
	if prev, ok := svc.PreviousReport(d2); !ok || prev != runs.reports["d"] {
		t.Errorf("PreviousReport(d) = (%p, %v), want d's first report %p", prev, ok, runs.reports["d"])
	}
	if cur, ok := svc.CurrentReport(d2); !ok || cur != runs.reports["d2"] {
		t.Errorf("CurrentReport(d) = (%p, %v), want d's second report %p", cur, ok, runs.reports["d2"])
	}
	if n := svc.reports.Len(); n != 2 {
		t.Errorf("pair cache holds %d pairs, want 2 (d with prev, plus one survivor)", n)
	}

	runs.size("huge", 400)
	huge := flatDoc("huge", "huge", 1)
	r, err := svc.AnalyzeFlow(ctx, huge)
	if err != nil || r != runs.reports["huge"] {
		t.Fatalf("AnalyzeFlow(huge) = (%p, %v), want the report served", r, err)
	}
	if _, ok := svc.CurrentReport(huge); ok {
		t.Error("a report over the budget must not be retained in the pair cache")
	}
	if n := svc.reports.Len(); n != 2 {
		t.Errorf("an unretained report evicted other pairs: %d left, want 2", n)
	}
}

// TestAnalyzeFlowBackground_LargeReportIsNotAddedToThePairCache: the governance
// scanner's sweep used to insert every library flow's report, evicting interactive
// users' pairs. A background report above backgroundReportCeiling is served and
// memoized elsewhere but not ADDED to the pair cache; a small one is, and an
// existing pair is still updated.
func TestAnalyzeFlowBackground_LargeReportIsNotAddedToThePairCache(t *testing.T) {
	runs := newFakeRuns()
	svc := newRound4Service(t, analysisReportBudget, runs)
	ctx := context.Background()

	bigFindings := int(backgroundReportCeiling>>10) + 1 // just over the ceiling
	runs.size("big", bigFindings)
	big := flatDoc("big", "big", 1)
	r, err := svc.AnalyzeFlowBackground(ctx, big)
	if err != nil || r == nil || len(r.Findings) != bigFindings {
		t.Fatalf("AnalyzeFlowBackground = (%v, %v)", r, err)
	}
	if _, ok := svc.CurrentReport(big); ok {
		t.Error("a background report over the ceiling was added to the pair cache")
	}
	if n := svc.reports.Len(); n != 0 {
		t.Errorf("pair cache holds %d pairs, want 0", n)
	}

	// Small background reports are tracked as before: the scanner's health-drop
	// check and the analyze handler's claim rely on it.
	runs.size("small", 10)
	small := flatDoc("small", "small", 1)
	if _, err := svc.AnalyzeFlowBackground(ctx, small); err != nil {
		t.Fatal(err)
	}
	if cur, ok := svc.CurrentReport(small); !ok || cur != runs.reports["small"] {
		t.Errorf("CurrentReport(small) = (%p, %v), want the background report", cur, ok)
	}

	if _, err := svc.AnalyzeFlow(ctx, big); err != nil {
		t.Fatal(err)
	}
	if cur, ok := svc.CurrentReport(big); !ok || cur != runs.reports["big"] {
		t.Fatalf("CurrentReport(big) after an interactive analysis = (%p, %v)", cur, ok)
	}
	runs.size("big2", bigFindings)
	big2 := flatDoc("big2", "big", 1)
	if _, err := svc.AnalyzeFlowBackground(ctx, big2); err != nil {
		t.Fatal(err)
	}
	if cur, ok := svc.CurrentReport(big2); !ok || cur != runs.reports["big2"] {
		t.Errorf("CurrentReport(big) after the background re-analysis = (%p, %v), want the new report", cur, ok)
	}
	if prev, ok := svc.PreviousReport(big2); !ok || prev != runs.reports["big"] {
		t.Errorf("PreviousReport(big) = (%p, %v), want the interactive report", prev, ok)
	}
}

// TestAnalyzeFlowFresh_ClaimOutlivesTheBudgetedPair: the dashboard-record claim used
// to ride on the report pair, so a pair the byte budget refused or evicted came back
// blank and every client re-fetch of exactly the largest flows was fresh again (a
// duplicate flow_analysis/history row and a re-fired webhook each time: the round-2
// bug). The claim lives in its own count-bounded side LRU; ReleaseRecord reopens it.
func TestAnalyzeFlowFresh_ClaimOutlivesTheBudgetedPair(t *testing.T) {
	runs := newFakeRuns()
	unit := analyzer.ReportCost(findingsReport("", 100))
	svc := newRound4Service(t, unit, runs) // room for exactly one 100-finding pair
	ctx := context.Background()

	runs.size("huge", 400)
	huge := flatDoc("huge", "huge", 1)
	r, fresh, err := svc.AnalyzeFlowFresh(ctx, huge)
	if err != nil || !fresh {
		t.Fatalf("first AnalyzeFlowFresh(huge) = (fresh=%v, %v), want fresh", fresh, err)
	}
	if _, ok := svc.CurrentReport(huge); ok {
		t.Fatal("test setup: the huge report must not be retained in the pair cache")
	}
	if r2, fresh, err := svc.AnalyzeFlowFresh(ctx, huge); err != nil || fresh || r2 != r {
		t.Errorf("second AnalyzeFlowFresh(huge) = (%p, fresh=%v, %v), want the same report, claimed", r2, fresh, err)
	}

	runs.size("a", 100)
	runs.size("b", 100)
	a, b := flatDoc("a", "a", 1), flatDoc("b", "b", 1)
	ra, fresh, err := svc.AnalyzeFlowFresh(ctx, a)
	if err != nil || !fresh {
		t.Fatalf("AnalyzeFlowFresh(a) = (fresh=%v, %v), want fresh", fresh, err)
	}
	if _, err := svc.AnalyzeFlow(ctx, b); err != nil {
		t.Fatal(err)
	}
	if _, ok := svc.CurrentReport(a); ok {
		t.Fatal("test setup: b should have evicted a's pair")
	}
	if _, fresh, err := svc.AnalyzeFlowFresh(ctx, a); err != nil || fresh {
		t.Errorf("AnalyzeFlowFresh(a) after eviction = (fresh=%v, %v), want still claimed", fresh, err)
	}

	svc.ReleaseRecord(a, ra)
	if _, fresh, err := svc.AnalyzeFlowFresh(ctx, a); err != nil || !fresh {
		t.Errorf("AnalyzeFlowFresh(a) after ReleaseRecord = (fresh=%v, %v), want fresh", fresh, err)
	}
}

// TestAnalyzeFlowFresh_ClaimMatchesARewalkOfUnchangedContent: an unretained report
// is re-walked on the next fetch (new pointer, same content), which is still the
// recorded report; an edit (new ContentHash) is fresh.
func TestAnalyzeFlowFresh_ClaimMatchesARewalkOfUnchangedContent(t *testing.T) {
	runs := newFakeRuns()
	svc := newRound4Service(t, analysisReportBudget, runs)
	ctx := context.Background()
	doc := flatDoc("rewalk", "rewalk", 1)

	// Every call hands out a NEW report object with the given content hash.
	hash := "content-v1"
	svc.run = func(_ context.Context, in analyzer.Input) *models.AnalysisReport {
		r := findingsReport(in.Doc.ID, 10)
		r.ContentHash = hash
		r.Stats = models.AnalysisStats{Warnings: 10, BlocksAnalyzed: 1}
		return r
	}
	r1, fresh, err := svc.AnalyzeFlowFresh(ctx, doc)
	if err != nil || !fresh {
		t.Fatalf("first fetch = (fresh=%v, %v), want fresh", fresh, err)
	}
	r2, fresh, err := svc.AnalyzeFlowFresh(ctx, doc)
	if err != nil || r2 == r1 {
		t.Fatalf("test setup: the re-walk must produce a new report object (%v)", err)
	}
	if fresh {
		t.Error("a re-walk of unchanged content must not be fresh (duplicate record)")
	}
	hash = "content-v2"
	if _, fresh, err := svc.AnalyzeFlowFresh(ctx, doc); err != nil || !fresh {
		t.Errorf("changed content = (fresh=%v, %v), want fresh", fresh, err)
	}
	// Without a content hash only the pointer identifies the report.
	hash = ""
	if _, fresh, err := svc.AnalyzeFlowFresh(ctx, doc); err != nil || !fresh {
		t.Errorf("hash-less new report = (fresh=%v, %v), want fresh", fresh, err)
	}
	if _, fresh, err := svc.AnalyzeFlowFresh(ctx, doc); err != nil || !fresh {
		t.Errorf("second hash-less new report = (fresh=%v, %v), want fresh (new pointer, no content identity)", fresh, err)
	}
}

// TestRecordClaims_CountBounded: claims are bounded by count, oldest out.
func TestRecordClaims_CountBounded(t *testing.T) {
	c := newRecordClaims(2)
	r := findingsReport("r", 1)
	c.take("a", r)
	c.take("b", r)
	c.take("c", r) // evicts a
	if !c.take("a", r) {
		t.Error("evicted claim a must be free again")
	}
	if c.take("c", r) {
		t.Error("claim c must still be held")
	}
	if c.order.Len() != 2 || len(c.index) != 2 {
		t.Errorf("claims hold %d/%d entries, want 2", c.order.Len(), len(c.index))
	}
}

// TestCountBlocks_IncludesNestedChildren: the gate's size check counts the blocks the
// analyzer will walk, nested children included.
func TestCountBlocks_IncludesNestedChildren(t *testing.T) {
	doc := makeAnalyzableDoc("nested",
		models.Block{ID: "b1", Type: models.BlockTypeLoop, Children: []models.Block{
			{ID: "b1a", Type: models.BlockTypeAction},
			{ID: "b1b", Type: models.BlockTypeCondition, Children: []models.Block{{ID: "b1b1", Type: models.BlockTypeAction}}},
		}},
		models.Block{ID: "b2", Type: models.BlockTypeAction},
	)
	doc.Subflows = append(doc.Subflows, models.Subflow{ID: "sf2", Blocks: []models.Block{{ID: "c1"}}})
	if got := countBlocks(doc); got != 6 {
		t.Errorf("countBlocks = %d, want 6", got)
	}
	if got := countBlocks(&models.FlowDocument{}); got != 0 {
		t.Errorf("countBlocks(empty) = %d, want 0", got)
	}
}

// gatedRuns is a fake analyzer whose large-flow walks block until released,
// counting how many run at once; small flows return immediately.
type gatedRuns struct {
	started chan string
	release chan struct{}
	running atomic.Int32
	peak    atomic.Int32
	small   atomic.Int32
}

func (g *gatedRuns) run(_ context.Context, in analyzer.Input) *models.AnalysisReport {
	doc := in.Doc
	if countBlocks(doc) < largeAnalysisBlocks {
		g.small.Add(1)
		return findingsReport(doc.ID, 1)
	}
	n := g.running.Add(1)
	for {
		p := g.peak.Load()
		if n <= p || g.peak.CompareAndSwap(p, n) {
			break
		}
	}
	g.started <- doc.ID
	<-g.release
	g.running.Add(-1)
	return findingsReport(doc.ID, 1)
}

// TestAnalyzeFlow_LargeAnalysesAreGated: at most largeAnalysisConcurrency uncached
// analyses of flows with >= largeAnalysisBlocks blocks run at once (the walk's
// transient state is ~2.5 KiB per block, so three concurrent 100k-block analyses
// were 0.5-1 GiB on top of the caches). Small flows and cache hits never wait; a
// waiter whose context is cancelled leaves with the context's error.
func TestAnalyzeFlow_LargeAnalysesAreGated(t *testing.T) {
	svc, err := NewAnalysisService(NilNotifier{}, nilSettings{}, nil, AnalysisOptions{})
	if err != nil {
		t.Fatalf("NewAnalysisService: %v", err)
	}
	// A private gate so other tests' analyses cannot share the slots.
	svc.largeSem = semaphore.NewWeighted(largeAnalysisConcurrency)
	ctx := context.Background()

	// A real analysis first, so the analyzer's cache holds a report for a large
	// flow: End blocks count for the gate but no rule runs on them, so the walk
	// is cheap.
	cachedBlocks := make([]models.Block, largeAnalysisBlocks)
	for i := range cachedBlocks {
		cachedBlocks[i] = models.Block{ID: fmt.Sprintf("end-%d", i), Type: models.BlockTypeEnd, RawType: "END", Name: "End", SubflowID: "main", LineNumber: i + 1}
	}
	cachedDoc := makeAnalyzableDoc("cached-large", cachedBlocks...)
	cachedReport, err := svc.AnalyzeFlowReadOnly(ctx, cachedDoc)
	if err != nil || cachedReport == nil {
		t.Fatalf("real analysis of the large flow: %v", err)
	}

	g := &gatedRuns{started: make(chan string, 8), release: make(chan struct{})}
	svc.run = g.run

	results := make(chan error, 3)
	for i := 0; i < 3; i++ {
		doc := flatDoc(fmt.Sprintf("large-%d", i), fmt.Sprintf("large-%d", i), largeAnalysisBlocks+5000)
		go func() {
			_, err := svc.AnalyzeFlowReadOnly(ctx, doc)
			results <- err
		}()
	}
	waitStarted := func(n int) {
		t.Helper()
		for i := 0; i < n; i++ {
			select {
			case <-g.started:
			case <-time.After(5 * time.Second):
				t.Fatalf("only %d large analyses started within 5s, want %d", i, n)
			}
		}
	}
	waitStarted(largeAnalysisConcurrency)
	select {
	case id := <-g.started:
		t.Fatalf("a third large analysis (%s) started while %d were running", id, largeAnalysisConcurrency)
	case <-time.After(150 * time.Millisecond):
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := svc.AnalyzeFlowReadOnly(ctx, flatDoc("tiny", "tiny", 3)); err != nil {
			t.Errorf("small flow: %v", err)
		}
		if r, err := svc.AnalyzeFlowReadOnly(ctx, cachedDoc); err != nil || r != cachedReport {
			t.Errorf("cached large flow = (%p, %v), want the cached report %p without waiting", r, err, cachedReport)
		}
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a small flow or a cache hit queued behind the large-analysis gate")
	}
	if g.small.Load() != 1 {
		t.Errorf("small flow ran %d times through the analyzer, want 1", g.small.Load())
	}

	waitCtx, cancel := context.WithCancel(ctx)
	waiterErr := make(chan error, 1)
	go func() {
		_, err := svc.AnalyzeFlowReadOnly(waitCtx, flatDoc("large-waiter", "large-waiter", largeAnalysisBlocks))
		waiterErr <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-waiterErr:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("cancelled waiter returned %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled waiter did not return")
	}

	close(g.release)
	waitStarted(1) // the third large analysis gets a slot once one frees up
	for i := 0; i < 3; i++ {
		select {
		case err := <-results:
			if err != nil {
				t.Errorf("large analysis: %v", err)
			}
		case <-time.After(5 * time.Second):
			t.Fatal("large analyses did not finish after release")
		}
	}
	if peak := g.peak.Load(); peak != largeAnalysisConcurrency {
		t.Errorf("peak concurrent large analyses = %d, want %d", peak, largeAnalysisConcurrency)
	}
	if g.running.Load() != 0 {
		t.Errorf("%d large analyses still counted as running", g.running.Load())
	}
}
