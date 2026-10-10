package analyzer

import (
	"context"
	"testing"

	"pad-core/cache"
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
		c := newReportCache(10, 0)
		c.testPutPath("file-a", "/flows/proj/a.txt", "h1", &models.AnalysisReport{FlowID: "ua"})
		c.testPutPath("file-b", "/flows/proj/b.txt", "h1", &models.AnalysisReport{FlowID: "ub"})
		c.testPutPath("other", "/elsewhere/c.txt", "h1", &models.AnalysisReport{FlowID: "uc"})

		c.testPutPath("folder", "/flows/proj", "h1", &models.AnalysisReport{FlowID: "uf"})

		reports := c.testAll()
		if len(reports) != 2 {
			t.Fatalf("want 2 entries (aggregate + unrelated file), got %d", len(reports))
		}
		if c.testGet("file-a", "h1") != nil || c.testGet("file-b", "h1") != nil {
			t.Error("per-file entries under the folder must be evicted")
		}
		if c.testGet("other", "h1") == nil {
			t.Error("unrelated file outside the folder must survive")
		}
	})

	t.Run("per-file entry evicts a folder aggregate covering it", func(t *testing.T) {
		c := newReportCache(10, 0)
		c.testPutPath("folder", "/flows/proj", "h1", &models.AnalysisReport{FlowID: "uf"})

		c.testPutPath("file-a", "/flows/proj/a.txt", "h1", &models.AnalysisReport{FlowID: "ua"})

		if c.testGet("folder", "h1") != nil {
			t.Error("folder aggregate covering the file must be evicted")
		}
		if got := len(c.testAll()); got != 1 {
			t.Fatalf("want 1 entry (the file), got %d", got)
		}
	})

	t.Run("sibling paths with a shared name prefix do not overlap", func(t *testing.T) {
		c := newReportCache(10, 0)
		c.testPutPath("f1", "/flows/proj", "h1", &models.AnalysisReport{FlowID: "u1"})
		c.testPutPath("f2", "/flows/proj2/x.txt", "h1", &models.AnalysisReport{FlowID: "u2"})
		if got := len(c.testAll()); got != 2 {
			t.Fatalf("/flows/proj must not cover /flows/proj2: want 2 entries, got %d", got)
		}
	})

	t.Run("path-less entries never overlap", func(t *testing.T) {
		c := newReportCache(10, 0)
		c.testPutPath("upload-1", "", "h1", &models.AnalysisReport{FlowID: "u1"})
		c.testPutPath("folder", "/flows/proj", "h1", &models.AnalysisReport{FlowID: "uf"})
		if got := len(c.testAll()); got != 2 {
			t.Fatalf("empty path must not overlap anything: want 2 entries, got %d", got)
		}
	})
}

func TestAnalysisCache_PutReplacesSameFlow(t *testing.T) {
	// Re-analyzing a flow after an edit or settings change produces a new hash.
	// Put must replace the flow's prior entry so dashboards (AllReports) count
	// each flow exactly once.
	c := newReportCache(10, 0)
	c.testPut("f1", "h1", &models.AnalysisReport{FlowID: "f1", DurationMs: 1})
	c.testPut("f1", "h2", &models.AnalysisReport{FlowID: "f1", DurationMs: 2})

	reports := c.testAll()
	if len(reports) != 1 {
		t.Fatalf("expected 1 report after re-Put with new hash, got %d", len(reports))
	}
	if reports[0].DurationMs != 2 {
		t.Errorf("expected newest report kept, got DurationMs=%d", reports[0].DurationMs)
	}
	if c.testGet("f1", "h1") != nil {
		t.Error("old hash entry must be gone")
	}
	if c.testGet("f1", "h2") == nil {
		t.Error("new hash entry must be present")
	}
	// Other flows are untouched.
	c.testPut("f2", "h1", &models.AnalysisReport{FlowID: "f2"})
	c.testPut("f1", "h3", &models.AnalysisReport{FlowID: "f1", DurationMs: 3})
	if len(c.testAll()) != 2 {
		t.Errorf("expected 2 reports (f1 latest + f2), got %d", len(c.testAll()))
	}
}

func TestCachedAnalyzer_StableIdentityAcrossReloads(t *testing.T) {
	// The same file re-loaded gets a fresh doc UUID and (after an edit) new
	// content. With path-stable identity the cache must hold ONE entry for it,
	// not one per load; otherwise the session analytics double-count the file.
	a := newTestAnalyzer()

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

	ctx := context.Background()
	a.Analyze(ctx, localIn(doc1, AllRules(), nil, nil))
	r2 := a.Analyze(ctx, localIn(doc2, AllRules(), nil, nil))

	if got := a.Reports(cache.Local()); len(got) != 1 || got[0] != r2 {
		t.Fatalf("same file across two loads must yield 1 entry (the newest), got %d", len(got))
	}

	// A different file coexists.
	doc3 := &models.FlowDocument{
		ID: "uuid-load-3", FilePath: "/flows/other.txt",
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b1}}},
	}
	a.Analyze(ctx, localIn(doc3, AllRules(), nil, nil))
	if got := len(a.Reports(cache.Local())); got != 2 {
		t.Fatalf("distinct files must keep distinct entries, got %d", got)
	}

	// A folder above both files replaces them, and a file under the folder replaces the folder: the analytics
	// never count the same findings twice.
	folder := &models.FlowDocument{
		ID: "uuid-folder", FilePath: "/flows", IsFolder: true,
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b1}}},
	}
	rf := a.Analyze(ctx, localIn(folder, AllRules(), nil, nil))
	if got := a.Reports(cache.Local()); len(got) != 1 || got[0] != rf {
		t.Fatalf("a folder must replace the files beneath it: %d entries", len(got))
	}
	r4 := a.Analyze(ctx, localIn(doc2, AllRules(), nil, nil))
	if got := a.Reports(cache.Local()); len(got) != 1 || got[0] != r4 {
		t.Fatalf("a file must replace the folder above it: %d entries", len(got))
	}
}
