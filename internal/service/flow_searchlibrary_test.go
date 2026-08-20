package service

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// filterRecorder wraps a StorageBackend and captures the FlowFilter each
// ListFlows call received, so tests can assert on filter shape (e.g. that a
// listing path asked for metadata only instead of pulling blob content).
type filterRecorder struct {
	*testutil.FakeBackend
	listFilters []interfaces.FlowFilter
}

func (f *filterRecorder) ListFlows(ctx context.Context, filter interfaces.FlowFilter) ([]*interfaces.FlowDocument, error) {
	f.listFilters = append(f.listFilters, filter)
	return f.FakeBackend.ListFlows(ctx, filter)
}

// seedSearchableFlow stores a flow whose content contains a block matching
// "needle", so SearchLibrary has a real hit to find.
func seedSearchableFlow(t *testing.T, backend *testutil.FakeBackend, id, name string) {
	t.Helper()
	doc := &models.FlowDocument{
		ID:   id,
		Name: name,
		Subflows: []models.Subflow{{
			ID:   id + "-sf",
			Name: "Main",
			Blocks: []models.Block{{
				ID:       id + "-b1",
				Name:     "Find the needle block",
				Type:     models.BlockTypeAction,
				RawType:  "Display.ShowMessage",
				Children: []models.Block{},
			}},
		}},
	}
	content, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal flow content: %v", err)
	}
	stored := &interfaces.FlowDocument{
		ID:      id,
		Name:    name,
		Content: content,
		Source:  "#Region \"Main\"\n#EndRegion",
		OwnerID: "user-1",
		Version: 3,
	}
	if err := backend.SaveFlow(context.Background(), stored); err != nil {
		t.Fatalf("seed flow: %v", err)
	}
}

func newSearchLibraryService(t *testing.T, backend *testutil.FakeBackend) (*FlowService, *filterRecorder) {
	t.Helper()
	rec := &filterRecorder{FakeBackend: backend}
	svc := NewFlowService(&testutil.CountingNotifier{}, nil, NewCloudDocumentProvider(rec), rec, nil, nil)
	return svc, rec
}

// TestSearchLibrary_ListsMetadataOnly is the behavior lock for the library
// search's enumerate phase: SearchLibrary only uses each listed flow's ID and
// name (content is re-resolved per flow via ResolveDoc), so the ListFlows call
// MUST set MetadataOnly — otherwise the Postgres backend backfills full blob
// content for up to maxLibrarySearchFlows flows that is then thrown away.
func TestSearchLibrary_ListsMetadataOnly(t *testing.T) {
	backend := testutil.NewFakeBackend()
	seedSearchableFlow(t, backend, "f1", "Flow One")
	seedSearchableFlow(t, backend, "f2", "Flow Two")
	svc, rec := newSearchLibraryService(t, backend)

	res, err := svc.SearchLibrary(context.Background(), "user-1", models.SearchQuery{Text: "needle", MaxResults: 10})
	if err != nil {
		t.Fatalf("SearchLibrary: %v", err)
	}
	if len(rec.listFilters) != 1 {
		t.Fatalf("expected exactly one ListFlows call, got %d", len(rec.listFilters))
	}
	if !rec.listFilters[0].MetadataOnly {
		t.Error("SearchLibrary's ListFlows filter must set MetadataOnly=true (content is resolved per-flow via ResolveDoc; the list phase would otherwise download blobs it discards)")
	}

	// The search itself must still find hits in both flows, stamped by flow.
	if len(res.Results) != 2 {
		t.Fatalf("expected 2 results (one per flow), got %d: %+v", len(res.Results), res.Results)
	}
	seen := map[string]bool{}
	for _, r := range res.Results {
		if r.FlowID == "" || r.FlowName == "" {
			t.Errorf("result missing flow stamp: %+v", r)
		}
		if !strings.Contains(strings.ToLower(r.MatchedText), "needle") && !strings.Contains(strings.ToLower(r.MatchedField), "needle") {
			t.Errorf("result does not reference the needle block: %+v", r)
		}
		seen[r.FlowID] = true
	}
	if !seen["f1"] || !seen["f2"] {
		t.Errorf("expected hits from both f1 and f2, got %v", seen)
	}
}
