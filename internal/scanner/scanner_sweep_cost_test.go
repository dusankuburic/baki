package scanner

import (
	"context"
	"encoding/json"
	"fmt"
	"testing"
	"time"

	"pad-analyzer/internal/notify"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// These tests pin what one sweep COSTS the storage backend, with the fake's
// read counters. A sweep used to page every flow's full content (plus a blob
// download per flow in blob mode) and issue two queries per flow before its
// skip check could fire; now it pages headers, batches the page's baselines
// and health, and loads a document only for a flow it re-analyzes.

// sweepCounts is a snapshot of the fake's read counters.
type sweepCounts struct {
	listFlows, contentBytes, loadFlow        int64
	baseline, baselineBatch, health, healthB int64
	loadOrg, ruleSets                        int64
}

func countsOf(b *healthBackend) sweepCounts {
	return sweepCounts{
		listFlows: b.ListFlowsCalls.Load(), contentBytes: b.ListFlowsContentBytes.Load(), loadFlow: b.LoadFlowCalls.Load(),
		baseline: b.GetFlowBaselineCalls.Load(), baselineBatch: b.GetFlowBaselinesBatchCalls.Load(),
		health: b.LoadFlowHealthCalls.Load(), healthB: b.LoadFlowHealthBatchCalls.Load(),
		loadOrg: b.LoadOrgCalls.Load(), ruleSets: b.RuleSetsByDigestCalls.Load(),
	}
}

// delta is the reads a sweep made: after minus before.
func (after sweepCounts) delta(before sweepCounts) sweepCounts {
	return sweepCounts{
		listFlows: after.listFlows - before.listFlows, contentBytes: after.contentBytes - before.contentBytes,
		loadFlow: after.loadFlow - before.loadFlow,
		baseline: after.baseline - before.baseline, baselineBatch: after.baselineBatch - before.baselineBatch,
		health: after.health - before.health, healthB: after.healthB - before.healthB,
		loadOrg: after.loadOrg - before.loadOrg, ruleSets: after.ruleSets - before.ruleSets,
	}
}

// sweep runs one sweep and returns the reads it made.
func sweep(h *skipHarness) sweepCounts {
	before := countsOf(h.b)
	h.s.ScanOnce(context.Background())
	return countsOf(h.b).delta(before)
}

// TestScanOnce_UnchangedFlowsAreNeitherLoadedNorQueriedPerFlow: the second
// sweep over an unchanged library reads no flow content at all (no LoadFlow,
// and the listing pages carry none) and no per-flow baseline or health — only
// the page's two batched reads.
func TestScanOnce_UnchangedFlowsAreNeitherLoadedNorQueriedPerFlow(t *testing.T) {
	ctx := context.Background()
	b := newBackend()
	const flows = 5
	for i := range flows {
		seedFlow(t, b, fmt.Sprintf("f%d", i))
	}
	// A mix of inputs: some flows have a baseline and recorded health, some
	// neither, so every key branch is exercised.
	if err := b.SetFlowBaseline(ctx, &storageif.FlowBaseline{FlowID: "f1", Keys: []string{"r1:b1"}}); err != nil {
		t.Fatal(err)
	}
	b.health["f2"] = &storageif.HealthSnapshot{HealthScore: 80}
	h := newSkipHarness(t, b)

	first := sweep(h)
	if first.loadFlow != flows {
		t.Fatalf("first sweep loaded %d documents, want %d (one per flow)", first.loadFlow, flows)
	}
	if first.baseline != 0 || first.health != 0 {
		t.Fatalf("first sweep made %d per-flow baseline and %d per-flow health reads, want the batched reads only", first.baseline, first.health)
	}
	if first.baselineBatch != 1 || first.healthB != 1 {
		t.Fatalf("first sweep made %d baseline-batch and %d health-batch reads, want 1 each (one page)", first.baselineBatch, first.healthB)
	}
	if first.contentBytes != 0 {
		t.Fatalf("the listing shipped %d bytes of flow content; a sweep pages headers only", first.contentBytes)
	}

	second := sweep(h)
	if second.loadFlow != 0 {
		t.Fatalf("second sweep over unchanged flows loaded %d documents, want 0", second.loadFlow)
	}
	if second.baseline != 0 || second.health != 0 {
		t.Fatalf("second sweep made %d per-flow baseline and %d per-flow health reads, want 0", second.baseline, second.health)
	}
	if second.baselineBatch != 1 || second.healthB != 1 {
		t.Fatalf("second sweep made %d baseline-batch and %d health-batch reads, want 1 each", second.baselineBatch, second.healthB)
	}
	if second.contentBytes != 0 {
		t.Fatalf("second sweep's listing shipped %d bytes of flow content, want 0", second.contentBytes)
	}
	if first.ruleSets != 0 || second.ruleSets != 0 {
		t.Fatalf("sweeps without recorded rule sets read rule sets %d and %d times, want 0", first.ruleSets, second.ruleSets)
	}
	for i := range flows {
		if n := h.runsOf(fmt.Sprintf("f%d", i)); n != 1 {
			t.Errorf("f%d analyzed %d times across two sweeps, want 1", i, n)
		}
	}
}

// TestScanOnce_LoadsOnlyTheFlowWhoseVersionChanged: a content save bumps the
// flow's version, which is what the skip key carries instead of the content;
// the next sweep loads and re-analyzes exactly that flow.
func TestScanOnce_LoadsOnlyTheFlowWhoseVersionChanged(t *testing.T) {
	ctx := context.Background()
	b := newBackend()
	for i := range 4 {
		seedFlow(t, b, fmt.Sprintf("f%d", i))
	}
	h := newSkipHarness(t, b)
	sweep(h)

	// Same bytes, new version: the fake's SaveFlow bumps Version on every
	// save, as flows.version does.
	before := b.Flows["f2"].Version
	if err := b.SaveFlow(ctx, &storageif.FlowDocument{ID: "f2", Name: "f2",
		Content: json.RawMessage(`{"id":"f2","subflows":[]}`)}); err != nil {
		t.Fatal(err)
	}
	if b.Flows["f2"].Version == before {
		t.Fatal("precondition: SaveFlow did not bump the version")
	}

	got := sweep(h)
	if got.loadFlow != 1 {
		t.Fatalf("sweep after one flow's version changed loaded %d documents, want exactly 1", got.loadFlow)
	}
	if got.contentBytes != 0 {
		t.Fatalf("the listing shipped %d bytes of flow content, want 0", got.contentBytes)
	}
	for i := range 4 {
		id := fmt.Sprintf("f%d", i)
		want := 1
		if id == "f2" {
			want = 2
		}
		if n := h.runsOf(id); n != want {
			t.Errorf("%s analyzed %d times, want %d", id, n, want)
		}
	}
	if again := sweep(h); again.loadFlow != 0 {
		t.Fatalf("the re-analyzed flow was loaded again (%d loads) with nothing changed", again.loadFlow)
	}
}

// TestScanOnce_PagesTheWholeLibraryByKeyset: a library larger than one page
// is walked with the id cursor (AfterID = the previous page's last id, never
// an Offset), header-only, and every flow is analyzed exactly once. A sweep
// that re-served the first page would stop short (ScanOnce's non-advancing
// page guard) and leave flows unanalyzed.
func TestScanOnce_PagesTheWholeLibraryByKeyset(t *testing.T) {
	b := newBackend()
	const flows = 250 // 100 per page: two full pages and a short one
	ids := make([]string, 0, flows)
	for i := range flows {
		id := fmt.Sprintf("flow-%03d", i)
		ids = append(ids, id)
		seedFlow(t, b, id)
	}
	h := newSkipHarness(t, b)

	got := sweep(h)
	if got.listFlows != 3 {
		t.Fatalf("sweep listed %d pages, want 3", got.listFlows)
	}
	if got.loadFlow != flows {
		t.Fatalf("sweep loaded %d documents, want %d", got.loadFlow, flows)
	}
	if got.baselineBatch != 3 || got.healthB != 3 {
		t.Fatalf("sweep batched baselines %d and health %d times, want 3 each (one per page)", got.baselineBatch, got.healthB)
	}
	for _, id := range ids {
		if n := h.runsOf(id); n != 1 {
			t.Errorf("%s analyzed %d times, want 1", id, n)
		}
	}
	wantAfter := []string{"", ids[99], ids[199]}
	for i, f := range b.ListFlowsFilterLog() {
		if f.AfterID != wantAfter[i] {
			t.Errorf("page %d AfterID = %q, want %q", i, f.AfterID, wantAfter[i])
		}
		if f.Offset != 0 {
			t.Errorf("page %d uses Offset %d; the walk is keyset-paged", i, f.Offset)
		}
		if !f.MetadataOnly || f.SortBy != storageif.FlowSortIDAsc || !f.AllFlows || f.Limit != 100 {
			t.Errorf("page %d filter = %+v, want MetadataOnly, FlowSortIDAsc, AllFlows, Limit 100", i, f)
		}
	}

	// Unchanged: the next sweep still walks every page (headers only) but
	// loads nothing.
	again := sweep(h)
	if again.listFlows != 3 || again.loadFlow != 0 {
		t.Fatalf("second sweep listed %d pages and loaded %d documents, want 3 and 0", again.listFlows, again.loadFlow)
	}
	s := h.s
	s.mu.Lock()
	keys := len(s.lastScan)
	s.mu.Unlock()
	if keys != flows {
		t.Fatalf("%d scan keys recorded after a complete sweep, want %d", keys, flows)
	}
}

// TestScanOnce_ReadsAnOrgRosterOncePerSweep: SSE recipients of an org flow's
// alert include every org member, and the roster is loaded once per org per
// sweep, not once per alert — and afresh on the next sweep.
func TestScanOnce_ReadsAnOrgRosterOncePerSweep(t *testing.T) {
	ctx := context.Background()
	const flows = 20
	b, reports := alertingFlows(t, flows)
	if err := b.SaveOrg(ctx, &storageif.Organisation{ID: "org-1", Name: "One",
		Members: []storageif.OrgMember{{UserID: "m1"}, {UserID: "m2"}}}); err != nil {
		t.Fatal(err)
	}

	global := newCapture(t)
	s := New(b, nil, analyzeReturning(reports), mustNotifier(t, global.srv.URL), 0, notify.AllowLoopbackTargets)
	sse := &fakeSSE{}
	s.SetEventNotifier(sse)
	s.ScanOnce(ctx)

	if n := b.LoadOrgCalls.Load(); n != 1 {
		t.Fatalf("%d alerts in one org loaded the org %d times, want once per sweep", flows, n)
	}
	pushes := map[string]int{}
	for _, c := range sse.calls() {
		pushes[c.userID]++
	}
	if pushes["m1"] != flows || pushes["m2"] != flows {
		t.Fatalf("members were pushed %v alerts, want %d each", pushes, flows)
	}

	// Next sweep: the cache is per sweep. Change one flow's baseline so it
	// alerts again (the others are deduped); the roster is read once more.
	if err := b.SetFlowBaseline(ctx, &storageif.FlowBaseline{FlowID: "flow-a0", Keys: []string{}}); err != nil {
		t.Fatal(err)
	}
	s.ScanOnce(ctx)
	if n := b.LoadOrgCalls.Load(); n != 2 {
		t.Fatalf("org loaded %d times after two sweeps, want 2 (once per sweep, not cached across sweeps)", n)
	}
	s.Stop()
}

// TestScanOnce_KeysTheScanOnTheDocumentItAnalyzed: a flow saved between the
// page listing and its load is analyzed at the new version and recorded under
// it, so the next sweep skips it rather than re-analyzing a flow nothing
// changed for since.
func TestScanOnce_KeysTheScanOnTheDocumentItAnalyzed(t *testing.T) {
	ctx := context.Background()
	b := newBackend()
	seedFlow(t, b, "f1")
	h := newSkipHarness(t, b)

	// Interpose on LoadFlow: the first load saves the flow first, as a user
	// editing during the sweep would.
	rb := &resaveOnLoadBackend{healthBackend: b}
	h.s.backend = rb
	h.s.ScanOnce(ctx)
	if n := h.runsOf("f1"); n != 1 {
		t.Fatalf("f1 analyzed %d times, want 1", n)
	}
	h.s.ScanOnce(ctx)
	if n := h.runsOf("f1"); n != 1 {
		t.Fatalf("f1 re-analyzed (%d runs) although the sweep already analyzed the version it was saved at", n)
	}
	if rb.resaved != 1 {
		t.Fatalf("resaved %d times, want 1", rb.resaved)
	}
}

// TestScanOnce_AStalledWalkIsIncompleteAndDoesNotPrune: a backend that
// ignores the id cursor re-serves the first page; the sweep stops at the
// non-advancing page instead of looping, and — not having seen every flow —
// must not prune the scan keys and dedup entries of the flows it never
// reached.
func TestScanOnce_AStalledWalkIsIncompleteAndDoesNotPrune(t *testing.T) {
	ctx := context.Background()
	b := newBackend()
	const flows = 250
	for i := range flows {
		seedFlow(t, b, fmt.Sprintf("flow-%03d", i))
	}
	h := newSkipHarness(t, b)
	h.s.ScanOnce(ctx) // complete: every flow has a scan key
	scanKeys := func() int {
		h.s.mu.Lock()
		defer h.s.mu.Unlock()
		return len(h.s.lastScan)
	}
	if n := scanKeys(); n != flows {
		t.Fatalf("%d scan keys after a complete sweep, want %d", n, flows)
	}

	stalled := &cursorIgnoringBackend{healthBackend: b}
	h.s.backend = stalled
	before := b.ListFlowsCalls.Load()
	h.s.ScanOnce(ctx)
	if pages := b.ListFlowsCalls.Load() - before; pages != 2 {
		t.Fatalf("a walk whose cursor is ignored listed %d pages, want 2 (the first, then the same page again, then stop)", pages)
	}
	if n := scanKeys(); n != flows {
		t.Fatalf("%d scan keys after an incomplete sweep, want all %d kept (only a complete sweep prunes)", n, flows)
	}
}

// cursorIgnoringBackend serves ListFlows as a backend that predates AfterID
// would: the cursor is dropped, so every page is the first one.
type cursorIgnoringBackend struct{ *healthBackend }

func (b *cursorIgnoringBackend) ListFlows(ctx context.Context, f storageif.FlowFilter) ([]*storageif.FlowDocument, error) {
	f.AfterID = ""
	return b.healthBackend.ListFlows(ctx, f)
}

// TestScanOnce_ABlockedPageReadIsBoundedAndNotRecorded: the listing and the
// batched baseline/health reads run under pageReadTimeout, a failed batch's
// per-flow fallback under perFlowScanTimeout — a hung read must not stall the
// sweep until Stop. A timed-out read is a failed read: its flows are analyzed
// without that check and keyed "" (not recorded); a timed-out listing ends
// the sweep.
func TestScanOnce_ABlockedPageReadIsBoundedAndNotRecorded(t *testing.T) {
	savedPage, savedFlow := pageReadTimeout, perFlowScanTimeout
	pageReadTimeout, perFlowScanTimeout = 150*time.Millisecond, 100*time.Millisecond
	t.Cleanup(func() { pageReadTimeout, perFlowScanTimeout = savedPage, savedFlow })

	for _, read := range []string{"baselines", "health", "list"} {
		t.Run(read, func(t *testing.T) {
			b := newBackend()
			seedFlow(t, b, "f1")
			seedFlow(t, b, "f2")
			h := newSkipHarness(t, b)
			h.s.backend = &blockingReadBackend{healthBackend: b, hang: read}

			start := time.Now()
			h.s.ScanOnce(context.Background())
			// The batch times out, then the fallback's two per-flow reads do.
			budget := pageReadTimeout + 2*perFlowScanTimeout
			if took := time.Since(start); took > 2*budget {
				t.Fatalf("sweep with a hung %s read took %v, want it bounded by the read timeouts (%v)", read, took, budget)
			}
			wantRuns := 1 // a failed read leaves the check out, not the flow
			if read == "list" {
				wantRuns = 0 // no page, nothing to scan
			}
			for _, id := range []string{"f1", "f2"} {
				if n := h.runsOf(id); n != wantRuns {
					t.Errorf("%s analyzed %d times, want %d", id, n, wantRuns)
				}
				if k := h.s.lastScanKey(id); k != "" {
					t.Errorf("%s recorded as scanned (key %q) although a page read failed", id, k)
				}
			}
		})
	}
}

// blockingReadBackend hangs one of a page's reads (hang: "baselines",
// "health" or "list"), batched and per-flow alike, until its context is done
// — a hung query — then reports the context's error.
type blockingReadBackend struct {
	*healthBackend
	hang string
}

func (b *blockingReadBackend) GetFlowBaselinesBatch(ctx context.Context, ids []string) (map[string]*storageif.FlowBaseline, error) {
	if b.hang == "baselines" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return b.healthBackend.GetFlowBaselinesBatch(ctx, ids)
}

func (b *blockingReadBackend) GetFlowBaseline(ctx context.Context, id string) (*storageif.FlowBaseline, error) {
	if b.hang == "baselines" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return b.healthBackend.GetFlowBaseline(ctx, id)
}

func (b *blockingReadBackend) LoadFlowHealthBatch(ctx context.Context, ids []string) (map[string]*storageif.HealthSnapshot, error) {
	if b.hang == "health" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return b.healthBackend.LoadFlowHealthBatch(ctx, ids)
}

func (b *blockingReadBackend) LoadFlowHealth(ctx context.Context, id string) (*storageif.HealthSnapshot, error) {
	if b.hang == "health" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return b.healthBackend.LoadFlowHealth(ctx, id)
}

func (b *blockingReadBackend) ListFlows(ctx context.Context, f storageif.FlowFilter) ([]*storageif.FlowDocument, error) {
	if b.hang == "list" {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	return b.healthBackend.ListFlows(ctx, f)
}

// TestScanOnce_AFailedBatchFallsBackToPerFlowReads: one undecodable row (or
// a timeout) fails a whole batch; the page then reads per flow, so every flow
// whose own read works still gets a real key and is recorded — only the
// genuinely unreadable flow is re-analyzed, unrecorded, on the next sweep.
func TestScanOnce_AFailedBatchFallsBackToPerFlowReads(t *testing.T) {
	for _, read := range []string{"baselines", "health"} {
		t.Run(read, func(t *testing.T) {
			b := newBackend()
			for _, id := range []string{"f1", "f2", "f3"} {
				seedFlow(t, b, id)
			}
			if err := b.SetFlowBaseline(context.Background(), &storageif.FlowBaseline{FlowID: "f1", Keys: []string{"r1:b1"}}); err != nil {
				t.Fatal(err)
			}
			b.health["f3"] = &storageif.HealthSnapshot{HealthScore: 80}
			h := newSkipHarness(t, b)
			h.s.backend = &batchFailingBackend{healthBackend: b, fail: read, brokenFlow: "f2"}

			first := sweep(h)
			if first.loadFlow != 3 {
				t.Fatalf("first sweep loaded %d documents, want 3", first.loadFlow)
			}
			perFlow := first.baseline
			if read == "health" {
				perFlow = first.health
			}
			if perFlow != 3 {
				t.Fatalf("a failed %s batch fell back to %d per-flow reads, want 3 (one per flow of the page)", read, perFlow)
			}
			for _, id := range []string{"f1", "f3"} {
				if h.s.lastScanKey(id) == "" {
					t.Errorf("%s not recorded as scanned although its own %s read succeeded", id, read)
				}
			}
			if k := h.s.lastScanKey("f2"); k != "" {
				t.Errorf("f2 recorded as scanned (key %q) although its %s read failed", k, read)
			}

			second := sweep(h)
			if second.loadFlow != 1 {
				t.Fatalf("second sweep loaded %d documents, want 1 (only the flow whose read keeps failing)", second.loadFlow)
			}
			if n := h.runsOf("f2"); n != 2 {
				t.Errorf("f2 analyzed %d times, want 2", n)
			}
		})
	}
}

// batchFailingBackend fails one batched read (fail: "baselines" or
// "health") outright and that read's per-flow form for brokenFlow only.
type batchFailingBackend struct {
	*healthBackend
	fail       string
	brokenFlow string
}

func (b *batchFailingBackend) GetFlowBaselinesBatch(ctx context.Context, ids []string) (map[string]*storageif.FlowBaseline, error) {
	if b.fail == "baselines" {
		return nil, fmt.Errorf("decode flow baseline %s keys: bad json", b.brokenFlow)
	}
	return b.healthBackend.GetFlowBaselinesBatch(ctx, ids)
}

func (b *batchFailingBackend) GetFlowBaseline(ctx context.Context, id string) (*storageif.FlowBaseline, error) {
	if b.fail == "baselines" && id == b.brokenFlow {
		b.GetFlowBaselineCalls.Add(1)
		return nil, fmt.Errorf("decode flow baseline %s keys: bad json", id)
	}
	return b.healthBackend.GetFlowBaseline(ctx, id)
}

func (b *batchFailingBackend) LoadFlowHealthBatch(ctx context.Context, ids []string) (map[string]*storageif.HealthSnapshot, error) {
	if b.fail == "health" {
		return nil, fmt.Errorf("scan flow health batch for %s: bad row", b.brokenFlow)
	}
	return b.healthBackend.LoadFlowHealthBatch(ctx, ids)
}

func (b *batchFailingBackend) LoadFlowHealth(ctx context.Context, id string) (*storageif.HealthSnapshot, error) {
	if b.fail == "health" && id == b.brokenFlow {
		b.LoadFlowHealthCalls.Add(1)
		return nil, fmt.Errorf("scan flow health for %s: bad row", id)
	}
	return b.healthBackend.LoadFlowHealth(ctx, id)
}

// resaveOnLoadBackend saves the flow (bumping its version) right before the
// first LoadFlow, simulating a save that lands between a sweep's page listing
// and its document load.
type resaveOnLoadBackend struct {
	*healthBackend
	resaved int
}

func (b *resaveOnLoadBackend) LoadFlow(ctx context.Context, id string) (*storageif.FlowDocument, error) {
	if b.resaved == 0 {
		b.resaved++
		if err := b.SaveFlow(ctx, &storageif.FlowDocument{ID: id, Name: id,
			Content: json.RawMessage(`{"id":"` + id + `","subflows":[]}`)}); err != nil {
			return nil, err
		}
	}
	return b.healthBackend.LoadFlow(ctx, id)
}
