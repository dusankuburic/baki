package analyzer

import (
	"testing"

	"pad-core/models"
)

func TestStableFlowID(t *testing.T) {
	t.Run("same FilePath gives same ID across re-parses", func(t *testing.T) {
		doc1 := &models.FlowDocument{ID: "uuid-1", FilePath: "/flows/main.txt"}
		doc2 := &models.FlowDocument{ID: "uuid-2", FilePath: "/flows/main.txt"}
		if StableFlowID(doc1) != StableFlowID(doc2) {
			t.Errorf("same path must give same ID: %s vs %s", StableFlowID(doc1), StableFlowID(doc2))
		}
	})

	t.Run("different paths give different IDs", func(t *testing.T) {
		doc1 := &models.FlowDocument{ID: "u", FilePath: "/flows/a.txt"}
		doc2 := &models.FlowDocument{ID: "u", FilePath: "/flows/b.txt"}
		if StableFlowID(doc1) == StableFlowID(doc2) {
			t.Error("different paths must give different IDs")
		}
	})

	t.Run("path-less doc falls back to doc.ID", func(t *testing.T) {
		doc := &models.FlowDocument{ID: "library-flow-7"}
		if got := StableFlowID(doc); got != "library-flow-7" {
			t.Errorf("fallback: want doc.ID, got %s", got)
		}
	})

	t.Run("parser-assigned StableID wins over doc.ID for path-less docs", func(t *testing.T) {
		doc := &models.FlowDocument{ID: "uuid-per-parse", StableID: "files-abc123"}
		if got := StableFlowID(doc); got != "files-abc123" {
			t.Errorf("want parser StableID, got %s", got)
		}
	})

	t.Run("agrees with StableFlowIDForPath", func(t *testing.T) {
		doc := &models.FlowDocument{ID: "u", FilePath: "/flows/x.txt"}
		if StableFlowID(doc) != StableFlowIDForPath("/flows/x.txt") {
			t.Error("doc-derived and path-derived IDs must match")
		}
	})

	t.Run("path is normalized (trailing slash, dot segments)", func(t *testing.T) {
		base := StableFlowIDForPath("/flows/proj")
		if StableFlowIDForPath("/flows/proj/") != base {
			t.Error("trailing slash must not change the identity")
		}
		if StableFlowIDForPath("/flows/./proj") != base {
			t.Error("dot segments must not change the identity")
		}
	})
}

func TestAnalysisCache_PutEvictsOverlappingPaths(t *testing.T) {
	// A folder-combined doc and its constituent files are overlapping
	// identities: whichever is analyzed last must replace the other, or the
	// dashboards would count the same findings twice.
	t.Run("folder aggregate evicts per-file entries beneath it", func(t *testing.T) {
		cache := NewAnalysisCache(10)
		cache.PutWithPath("file-a", "/flows/proj/a.txt", "h1", &models.AnalysisReport{FlowID: "ua"})
		cache.PutWithPath("file-b", "/flows/proj/b.txt", "h1", &models.AnalysisReport{FlowID: "ub"})
		cache.PutWithPath("other", "/elsewhere/c.txt", "h1", &models.AnalysisReport{FlowID: "uc"})

		cache.PutWithPath("folder", "/flows/proj", "h1", &models.AnalysisReport{FlowID: "uf"})

		reports := cache.AllReports()
		if len(reports) != 2 {
			t.Fatalf("want 2 entries (aggregate + unrelated file), got %d", len(reports))
		}
		if cache.Get("file-a", "h1") != nil || cache.Get("file-b", "h1") != nil {
			t.Error("per-file entries under the folder must be evicted")
		}
		if cache.Get("other", "h1") == nil {
			t.Error("unrelated file outside the folder must survive")
		}
	})

	t.Run("per-file entry evicts a folder aggregate covering it", func(t *testing.T) {
		cache := NewAnalysisCache(10)
		cache.PutWithPath("folder", "/flows/proj", "h1", &models.AnalysisReport{FlowID: "uf"})

		cache.PutWithPath("file-a", "/flows/proj/a.txt", "h1", &models.AnalysisReport{FlowID: "ua"})

		if cache.Get("folder", "h1") != nil {
			t.Error("folder aggregate covering the file must be evicted")
		}
		if got := len(cache.AllReports()); got != 1 {
			t.Fatalf("want 1 entry (the file), got %d", got)
		}
	})

	t.Run("sibling paths with a shared name prefix do not overlap", func(t *testing.T) {
		cache := NewAnalysisCache(10)
		cache.PutWithPath("f1", "/flows/proj", "h1", &models.AnalysisReport{FlowID: "u1"})
		cache.PutWithPath("f2", "/flows/proj2/x.txt", "h1", &models.AnalysisReport{FlowID: "u2"})
		if got := len(cache.AllReports()); got != 2 {
			t.Fatalf("/flows/proj must not cover /flows/proj2: want 2 entries, got %d", got)
		}
	})

	t.Run("path-less entries never overlap", func(t *testing.T) {
		cache := NewAnalysisCache(10)
		cache.PutWithPath("upload-1", "", "h1", &models.AnalysisReport{FlowID: "u1"})
		cache.PutWithPath("folder", "/flows/proj", "h1", &models.AnalysisReport{FlowID: "uf"})
		if got := len(cache.AllReports()); got != 2 {
			t.Fatalf("empty path must not overlap anything: want 2 entries, got %d", got)
		}
	})
}

func TestAnalysisCache_PutReplacesSameFlow(t *testing.T) {
	// Re-analyzing a flow after an edit or settings change produces a new hash.
	// Put must replace the flow's prior entry so dashboards (AllReports) count
	// each flow exactly once.
	cache := NewAnalysisCache(10)
	cache.Put("f1", "h1", &models.AnalysisReport{FlowID: "f1", DurationMs: 1})
	cache.Put("f1", "h2", &models.AnalysisReport{FlowID: "f1", DurationMs: 2})

	reports := cache.AllReports()
	if len(reports) != 1 {
		t.Fatalf("expected 1 report after re-Put with new hash, got %d", len(reports))
	}
	if reports[0].DurationMs != 2 {
		t.Errorf("expected newest report kept, got DurationMs=%d", reports[0].DurationMs)
	}
	if cache.Get("f1", "h1") != nil {
		t.Error("old hash entry must be gone")
	}
	if cache.Get("f1", "h2") == nil {
		t.Error("new hash entry must be present")
	}
	// Other flows are untouched.
	cache.Put("f2", "h1", &models.AnalysisReport{FlowID: "f2"})
	cache.Put("f1", "h3", &models.AnalysisReport{FlowID: "f1", DurationMs: 3})
	if len(cache.AllReports()) != 2 {
		t.Errorf("expected 2 reports (f1 latest + f2), got %d", len(cache.AllReports()))
	}
}

func TestCachedAnalysis_StableIdentityAcrossReloads(t *testing.T) {
	// The same file re-loaded gets a fresh doc UUID and (after an edit) new
	// content. With path-stable identity the cache must hold ONE entry for it,
	// not one per load — otherwise dashboards double-count the file.
	DefaultCache = NewAnalysisCache(10)

	b1 := makeBlock("b1", "Set X", models.BlockTypeAction, "SetVariable.Set", 0)
	b1.SubflowID = "sf1"
	doc1 := &models.FlowDocument{
		ID: "uuid-load-1", FilePath: "/flows/main.txt",
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b1}}},
	}
	// Same file, re-parsed (new UUID), content edited.
	b2 := makeBlock("b1", "Set Y", models.BlockTypeAction, "SetVariable.Set", 0)
	b2.SubflowID = "sf1"
	doc2 := &models.FlowDocument{
		ID: "uuid-load-2", FilePath: "/flows/main.txt",
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b2}}},
	}

	CachedAnalysis(doc1, AllRules(), nil, nil)
	CachedAnalysis(doc2, AllRules(), nil, nil)

	if got := len(DefaultCache.AllReports()); got != 1 {
		t.Fatalf("same file across two loads must yield 1 cache entry, got %d", got)
	}

	// A different file coexists.
	doc3 := &models.FlowDocument{
		ID: "uuid-load-3", FilePath: "/flows/other.txt",
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b1}}},
	}
	CachedAnalysis(doc3, AllRules(), nil, nil)
	if got := len(DefaultCache.AllReports()); got != 2 {
		t.Fatalf("distinct files must keep distinct entries, got %d", got)
	}
}
