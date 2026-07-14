package analyzer

import (
	"testing"

	"pad-core/models"
)

func TestAnalysisCache(t *testing.T) {
	t.Run("cache miss returns nil", func(t *testing.T) {
		cache := NewAnalysisCache(10)
		got := cache.Get("flow1", "hash1")
		if got != nil {
			t.Fatal("expected nil on cache miss")
		}
	})

	t.Run("cache put then get returns report", func(t *testing.T) {
		cache := NewAnalysisCache(10)
		report := &models.AnalysisReport{FlowID: "flow1", DurationMs: 42}
		cache.Put("flow1", "hash1", report)
		got := cache.Get("flow1", "hash1")
		if got == nil {
			t.Fatal("expected cached report, got nil")
		}
		if got.DurationMs != 42 {
			t.Errorf("DurationMs = %d, want 42", got.DurationMs)
		}
	})

	t.Run("different hash returns nil", func(t *testing.T) {
		cache := NewAnalysisCache(10)
		report := &models.AnalysisReport{FlowID: "flow1"}
		cache.Put("flow1", "hash1", report)
		got := cache.Get("flow1", "hash2")
		if got != nil {
			t.Fatal("expected nil for different hash")
		}
	})

	t.Run("eviction drops oldest entry", func(t *testing.T) {
		cache := NewAnalysisCache(2)
		cache.Put("f1", "h1", &models.AnalysisReport{FlowID: "f1", DurationMs: 1})
		cache.Put("f2", "h1", &models.AnalysisReport{FlowID: "f2", DurationMs: 2})
		cache.Put("f3", "h1", &models.AnalysisReport{FlowID: "f3", DurationMs: 3})
		if cache.Get("f1", "h1") != nil {
			t.Fatal("expected f1 to be evicted")
		}
		if cache.Get("f3", "h1") == nil {
			t.Fatal("expected f3 to be present")
		}
	})
}

func TestFlowHash(t *testing.T) {
	t.Run("same flow produces same hash", func(t *testing.T) {
		b := makeBlock("b1", "Set X", models.BlockTypeAction, "SetVariable.Set", 0)
		b.SubflowID = "sf1"
		doc1 := &models.FlowDocument{ID: "t1", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		doc2 := &models.FlowDocument{ID: "t2", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		h1 := FlowHash(doc1)
		h2 := FlowHash(doc2)
		if h1 != h2 {
			t.Errorf("hashes differ for same content: %s vs %s", h1, h2)
		}
	})

	t.Run("different content produces different hash", func(t *testing.T) {
		b1 := makeBlock("b1", "Set X", models.BlockTypeAction, "SetVariable.Set", 0)
		b1.SubflowID = "sf1"
		b2 := makeBlock("b1", "Set Y", models.BlockTypeAction, "SetVariable.Set", 0)
		b2.SubflowID = "sf1"
		doc1 := &models.FlowDocument{ID: "t", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b1}}}}
		doc2 := &models.FlowDocument{ID: "t", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b2}}}}
		h1 := FlowHash(doc1)
		h2 := FlowHash(doc2)
		if h1 == h2 {
			t.Error("expected different hashes for different content")
		}
	})

	// Regression: a block with several properties must hash identically on every
	// call. Before sorting the property keys, Go's randomized map iteration made
	// FlowHash non-deterministic, so the cache never hit for real flows.
	t.Run("hash is deterministic across many calls (multi-property block)", func(t *testing.T) {
		b := makeBlock("b1", "HTTP Request", models.BlockTypeAction, "HTTPClient.InvokeService", 0)
		b.SubflowID = "sf1"
		b.Properties = map[string]string{
			"url": "https://x", "method": "GET", "timeout": "30", "body": "{}", "header": "h",
		}
		doc := &models.FlowDocument{ID: "t", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		first := FlowHash(doc)
		for i := range 100 {
			if h := FlowHash(doc); h != first {
				t.Fatalf("FlowHash non-deterministic: call %d gave %s, want %s", i, h, first)
			}
		}
	})

	// Regression: the parser mints fresh UUIDs for every block/subflow on each
	// parse. FlowHash must NOT fold those IDs in, or the analysis cache never
	// hits across re-parses of byte-identical source. Two docs with identical
	// content but different minted IDs must hash equally.
	t.Run("stable across re-parses (different UUIDs, same content)", func(t *testing.T) {
		mk := func(blockID, sfID string) *models.FlowDocument {
			b := makeBlock(blockID, "Set X", models.BlockTypeAction, "SetVariable.Set", 0)
			b.SubflowID = sfID
			return &models.FlowDocument{
				ID:       "t",
				Subflows: []models.Subflow{{ID: sfID, Name: "Main", Blocks: []models.Block{*b}}},
			}
		}
		h1 := FlowHash(mk("uuid-aaaa", "sf-1111"))
		h2 := FlowHash(mk("uuid-bbbb", "sf-2222"))
		if h1 != h2 {
			t.Fatalf("FlowHash changed across re-parses: %s vs %s\n"+
				"Block/subflow UUIDs must not participate in the content hash.", h1, h2)
		}
	})
}

func TestCustomRule(t *testing.T) {
	t.Run("matches rawType pattern", func(t *testing.T) {
		cr, err := NewCustomRule(CustomRuleConfig{
			ID:           "custom-1",
			Name:         "No HTTP GET",
			Description:  "HTTP GET actions are not allowed",
			Severity:     "warning",
			Category:     "Security",
			RawTypeMatch: "HTTPClient\\.Invoke",
			Suggestion:   "Use POST instead of GET.",
		})
		if err != nil {
			t.Fatal(err)
		}
		b := makeBlock("b1", "HTTP Request", models.BlockTypeAction, "HTTPClient.InvokeService", 0)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := cr.Check(b, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
		if got[0].RuleID != "custom-1" {
			t.Errorf("ruleID = %q", got[0].RuleID)
		}
	})

	t.Run("non-matching block emits no finding", func(t *testing.T) {
		cr, err := NewCustomRule(CustomRuleConfig{
			ID:           "custom-1",
			Name:         "No HTTP GET",
			Description:  "HTTP GET actions are not allowed",
			RawTypeMatch: "HTTPClient\\.Invoke",
		})
		if err != nil {
			t.Fatal(err)
		}
		b := makeBlock("b1", "Set variable", models.BlockTypeAction, "SetVariable.Set", 0)
		b.SubflowID = "sf1"
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := cr.Check(b, ctx)
		if len(got) != 0 {
			t.Fatalf("expected 0 findings, got %d", len(got))
		}
	})

	t.Run("propertyHas filter works", func(t *testing.T) {
		cr, err := NewCustomRule(CustomRuleConfig{
			ID:           "custom-2",
			Name:         "HTTP without body",
			Description:  "HTTP request with empty body",
			RawTypeMatch: "HTTPClient\\..*",
			PropertyHas:  map[string]string{"method": "GET"},
		})
		if err != nil {
			t.Fatal(err)
		}
		b := makeBlock("b1", "HTTP GET", models.BlockTypeAction, "HTTPClient.InvokeService", 0)
		b.SubflowID = "sf1"
		b.Properties = map[string]string{"method": "GET", "url": "https://example.com"}
		flow := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		ctx := buildContext(flow, nil)
		got := cr.Check(b, ctx)
		if len(got) != 1 {
			t.Fatalf("expected 1 finding, got %d", len(got))
		}
	})
}

func TestCachedAnalysis(t *testing.T) {
	t.Run("second call returns cached result", func(t *testing.T) {
		DefaultCache = NewAnalysisCache(10)
		b := makeBlock("b1", "Set X", models.BlockTypeAction, "SetVariable.Set", 0)
		b.SubflowID = "sf1"
		doc := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		r1 := CachedAnalysis(doc, AllRules(), nil, nil)
		r2 := CachedAnalysis(doc, AllRules(), nil, nil)
		if r1 == nil || r2 == nil {
			t.Fatal("expected non-nil reports")
		}
		if r1.DurationMs != r2.DurationMs {
			t.Errorf("second call should return cached report")
		}
	})

	// Regression: a flow whose blocks carry multiple properties must hit the
	// cache on the second call. This relies on FlowHash being deterministic;
	// before the key-sort fix the second call recomputed instead of hitting.
	t.Run("multi-property flow hits cache on second call", func(t *testing.T) {
		DefaultCache = NewAnalysisCache(10)
		b := makeBlock("b1", "HTTP Request", models.BlockTypeAction, "HTTPClient.InvokeService", 0)
		b.SubflowID = "sf1"
		b.Properties = map[string]string{"url": "https://x", "method": "GET", "timeout": "30", "body": "{}"}
		doc := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		r1 := CachedAnalysis(doc, AllRules(), nil, nil)
		r2 := CachedAnalysis(doc, AllRules(), nil, nil)
		// On a cache hit, CachedAnalysis returns the exact stored pointer.
		if r1 != r2 {
			t.Error("expected the same cached report pointer on the second call (cache miss implies non-deterministic hash)")
		}
	})
}

func TestComputeSubflowHashesDeterministic(t *testing.T) {
	// Regression: ComputeSubflowHashes must be stable across calls for a subflow
	// containing multi-property blocks (same map-iteration-order bug as FlowHash).
	b := makeBlock("b1", "HTTP Request", models.BlockTypeAction, "HTTPClient.InvokeService", 0)
	b.Properties = map[string]string{"a": "1", "b": "2", "c": "3", "d": "4", "e": "5"}
	doc := &models.FlowDocument{ID: "t", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
	first := ComputeSubflowHashes(doc)
	if len(first) != 1 {
		t.Fatalf("expected 1 subflow hash, got %d", len(first))
	}
	for i := range 100 {
		got := ComputeSubflowHashes(doc)
		if got[0].Hash != first[0].Hash {
			t.Fatalf("computeSubflowHash non-deterministic at call %d: %s != %s", i, got[0].Hash, first[0].Hash)
		}
	}
}

func TestRuleProfiling(t *testing.T) {
	t.Run("report contains rule profiles", func(t *testing.T) {
		b := makeBlock("b1", "Set X", models.BlockTypeVariable, "Variables.SetVariable", 0)
		b.SubflowID = "sf1"
		b.Properties = map[string]string{"_output": "X", "_value": "%X%"}
		doc := &models.FlowDocument{ID: "test", Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{*b}}}}
		report := RunAnalysis(doc, AllRules(), nil, nil)
		if len(report.RuleProfiles) == 0 {
			t.Fatal("expected rule profiles in report")
		}
		found := false
		for _, rp := range report.RuleProfiles {
			if rp.RuleID == "redundant-action" {
				found = true
				if rp.RuleName == "" {
					t.Error("expected non-empty rule name")
				}
			}
		}
		if !found {
			t.Error("expected redundant-action profile")
		}
	})
}

func TestAnalysisCache_Clear(t *testing.T) {
	cache := NewAnalysisCache(10)
	cache.Put("f1", "h1", &models.AnalysisReport{FlowID: "f1"})
	cache.Put("f2", "h1", &models.AnalysisReport{FlowID: "f2"})
	if len(cache.AllReports()) != 2 {
		t.Fatalf("setup: expected 2 reports, got %d", len(cache.AllReports()))
	}

	cache.Clear()

	if got := len(cache.AllReports()); got != 0 {
		t.Errorf("after Clear: expected 0 reports, got %d", got)
	}
	if cache.Get("f1", "h1") != nil {
		t.Error("after Clear: expected cache miss for f1")
	}
	// Cache stays usable after clearing.
	cache.Put("f3", "h1", &models.AnalysisReport{FlowID: "f3"})
	if cache.Get("f3", "h1") == nil {
		t.Error("expected f3 present after re-Put following Clear")
	}
}
