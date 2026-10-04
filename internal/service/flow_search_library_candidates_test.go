package service

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/cache"
	"pad-core/models"
)

// scanRecorder records the pushdown's candidate limit and the peak number of
// concurrent LoadFlow calls (each held open briefly so the fan-out overlaps).
type scanRecorder struct {
	*testutil.FakeBackend
	mu          sync.Mutex
	limits      []int
	inFlight    atomic.Int32
	maxInFlight atomic.Int32
}

func (r *scanRecorder) SearchFlowContents(ctx context.Context, filter interfaces.FlowFilter, needle string, limit int) ([]*interfaces.FlowDocument, error) {
	r.mu.Lock()
	r.limits = append(r.limits, limit)
	r.mu.Unlock()
	return r.FakeBackend.SearchFlowContents(ctx, filter, needle, limit)
}

func (r *scanRecorder) LoadFlow(ctx context.Context, id string) (*interfaces.FlowDocument, error) {
	n := r.inFlight.Add(1)
	defer r.inFlight.Add(-1)
	for {
		cur := r.maxInFlight.Load()
		if n <= cur || r.maxInFlight.CompareAndSwap(cur, n) {
			break
		}
	}
	time.Sleep(5 * time.Millisecond)
	return r.FakeBackend.LoadFlow(ctx, id)
}

// seedNeedleFlow stores a flow whose content mentions the needle, newest
// first by index so the pushdown's recency order is deterministic.
func seedNeedleFlow(t *testing.T, backend *testutil.FakeBackend, i int) string {
	t.Helper()
	id := fmt.Sprintf("flow-%03d", i)
	doc := &models.FlowDocument{ID: id, Name: "Flow " + id, Subflows: []models.Subflow{{
		ID: id + "-sf", Name: "Main",
		Blocks: []models.Block{{ID: id + "-b1", Name: "Find the needle block", Type: models.BlockTypeAction, RawType: "Display.ShowMessage", Children: []models.Block{}}},
	}}}
	content, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := backend.SaveFlow(context.Background(), &interfaces.FlowDocument{
		ID: id, Name: doc.Name, Content: content, OwnerID: "user-1",
		UpdatedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(-time.Duration(i) * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

// TestSearchLibrary_CandidatesDoNotEnterTheCaches: a library search over 120
// matching flows asks the pushdown for at most 50 candidates, resolves them
// with a fan-out of at most 4, and leaves BOTH caches holding exactly what
// they held before — the hot document (served from the cache, its index
// cached) and nothing else — instead of cycling every candidate through them.
func TestSearchLibrary_CandidatesDoNotEnterTheCaches(t *testing.T) {
	backend := testutil.NewFakeBackend()
	for i := 0; i < 120; i++ {
		seedNeedleFlow(t, backend, i)
	}
	rec := &scanRecorder{FakeBackend: backend}
	provider := NewCloudDocumentProvider(rec)
	svc := NewFlowService(&testutil.CountingNotifier{}, nil, provider, rec, nil, nil)
	ctx := context.Background()

	hot := "flow-000" // the newest: always within the candidate cut
	if _, err := provider.ResolveDoc(ctx, hot); err != nil {
		t.Fatal(err)
	}
	if provider.cache.Len() != 1 {
		t.Fatalf("premise: %d cached documents before the search, want 1", provider.cache.Len())
	}
	rec.LoadFlowCalls.Store(0)

	res, err := svc.SearchLibrary(ctx, "user-1", models.SearchQuery{Text: "needle", MaxResults: 100})
	if err != nil {
		t.Fatalf("SearchLibrary: %v", err)
	}

	if len(rec.limits) != 1 || rec.limits[0] != 50 {
		t.Errorf("pushdown candidate limits = %v, want one call capped at 50", rec.limits)
	}
	flows := map[string]bool{}
	for _, r := range res.Results {
		flows[r.FlowID] = true
	}
	if len(flows) == 0 || len(flows) > 50 || !flows[hot] {
		t.Errorf("results span %d flows (hot included: %v), want 1..50 including the hot one", len(flows), flows[hot])
	}
	if got := rec.LoadFlowCalls.Load(); got != int64(len(flows))-1 {
		t.Errorf("%d backend loads for %d candidate flows, want every candidate but the cached hot one loaded", got, len(flows))
	}
	if got := rec.maxInFlight.Load(); got > 4 {
		t.Errorf("peak concurrent candidate loads = %d, want at most the fan-out of 4", got)
	}
	if provider.cache.Len() != 1 {
		t.Errorf("the search left %d documents in the document cache, want only the hot one", provider.cache.Len())
	}
	if got := svc.idxCache.(*cache.SizedLRU).Len(); got != 1 {
		t.Errorf("the search left %d search indexes cached, want only the hot document's", got)
	}
}
