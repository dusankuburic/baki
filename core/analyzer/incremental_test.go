package analyzer

import (
	"fmt"
	"testing"

	"pad-core/models"
)

func TestAnalyzeRuleDependencies(t *testing.T) {
	t.Run("returns_dependencies", func(t *testing.T) {
		da := AnalyzeRuleDependencies()
		if len(da.Dependencies) == 0 {
			t.Fatal("expected some dependencies")
		}
		for _, d := range da.Dependencies {
			if d.FromRuleID == "" || d.ToRuleID == "" {
				t.Errorf("dependency has empty rule IDs: %+v", d)
			}
			if d.Reason == "" {
				t.Errorf("dependency missing reason: %+v", d)
			}
		}
	})

	t.Run("topo_order_contains_all_rules", func(t *testing.T) {
		da := AnalyzeRuleDependencies()
		rules := AllRules()
		if len(da.TopoOrder) != len(rules) {
			t.Fatalf("topo order has %d entries, expected %d rules", len(da.TopoOrder), len(rules))
		}
		seen := map[string]bool{}
		for _, id := range da.TopoOrder {
			if seen[id] {
				t.Errorf("duplicate rule in topo order: %s", id)
			}
			seen[id] = true
		}
	})

	t.Run("no_cycles", func(t *testing.T) {
		da := AnalyzeRuleDependencies()
		if len(da.Cycles) > 0 {
			t.Errorf("expected no cycles, got %d: %v", len(da.Cycles), da.Cycles)
		}
	})
}

// TestDetectCycles_ReportsCycle guards detectCycles' behavioral contract: when
// the dependency graph contains a cycle, detectCycles must report a cycle
// covering every node on the cyclic path. Since the Tarjan SCC swap (was M9),
// each reported cycle is the SCC's node set in a canonical (sorted) order — no
// duplicate leading element — so we additionally assert no entry repeats.
func TestDetectCycles_ReportsCycle(t *testing.T) {
	// 3-cycle: a → b → c → a, plus an unrelated acyclic node d → (nothing).
	adj := map[string][]string{
		"a": {"b"},
		"b": {"c"},
		"c": {"a"},
	}
	nodes := map[string]bool{"a": true, "b": true, "c": true, "d": true}

	cycles := detectCycles(adj, nodes)
	if len(cycles) == 0 {
		t.Fatal("expected at least one cycle for {a→b→c→a}, got none")
	}

	// Find a cycle that covers all 3 nodes (rotations are equivalent).
	want := map[string]bool{"a": true, "b": true, "c": true}
	var found bool
	for _, cyc := range cycles {
		// Clean-format invariant: no node repeats within a reported cycle.
		seen := map[string]bool{}
		for _, n := range cyc {
			if seen[n] {
				t.Errorf("reported cycle has a duplicate node %q: %v", n, cyc)
			}
			seen[n] = true
		}
		if len(seen) == len(want) {
			match := true
			for k := range want {
				if !seen[k] {
					match = false
					break
				}
			}
			if match {
				found = true
			}
		}
	}
	if !found {
		t.Errorf("no reported cycle covered all of {a,b,c}; cycles=%v", cycles)
	}
}

// TestDetectCycles_CanonicalOrder locks the post-Tarjan determinism: the same
// graph always yields the same cycle node set in sorted order, regardless of
// which rotation the back-edge closes on.
func TestDetectCycles_CanonicalOrder(t *testing.T) {
	build := func() map[string][]string {
		return map[string][]string{
			"x": {"y"},
			"y": {"z"},
			"z": {"x"},
		}
	}
	nodes := map[string]bool{"x": true, "y": true, "z": true}

	// Run several times — the SCC node set + sort must be identical every time.
	first := detectCycles(build(), nodes)
	if len(first) != 1 {
		t.Fatalf("expected exactly 1 cycle, got %d: %v", len(first), first)
	}
	want := []string{"x", "y", "z"}
	if !equalStringSlice(first[0], want) {
		t.Errorf("cycle = %v, want canonical sorted %v", first[0], want)
	}
	for i := 0; i < 25; i++ {
		got := detectCycles(build(), nodes)
		if len(got) != 1 || !equalStringSlice(got[0], first[0]) {
			t.Fatalf("iter %d: cycle drifted to %v (want %v)", i, got, first)
		}
	}
}

// TestDetectCycles_SelfLoop reports a single-node SCC with a self-edge as a cycle.
func TestDetectCycles_SelfLoop(t *testing.T) {
	adj := map[string][]string{"s": {"s"}}
	nodes := map[string]bool{"s": true}
	cycles := detectCycles(adj, nodes)
	if len(cycles) != 1 || len(cycles[0]) != 1 || cycles[0][0] != "s" {
		t.Errorf("expected single self-loop cycle [[s]], got %v", cycles)
	}
}

func equalStringSlice(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestDetectCycles_AcyclicGraphReturnsNone is the negative control: a DAG must
// not produce any cycles. Guards against false-positive regressions.
func TestDetectCycles_AcyclicGraphReturnsNone(t *testing.T) {
	adj := map[string][]string{
		"a": {"b"},
		"b": {"c"},
		"c": {},
	}
	nodes := map[string]bool{"a": true, "b": true, "c": true}
	cycles := detectCycles(adj, nodes)
	if len(cycles) != 0 {
		t.Errorf("expected 0 cycles for acyclic graph, got %d: %v", len(cycles), cycles)
	}
}

func TestComputeSubflowHashes(t *testing.T) {
	t.Run("generates_hashes", func(t *testing.T) {
		doc := makeFlowDoc("f1", "Test", []models.Subflow{
			makeSubflow("sf1", "Main",
				makeBlock("b1", "Action", models.BlockTypeAction, "Action.Invoke", 0),
			),
			makeSubflow("sf2", "Helper",
				makeBlock("b2", "Loop", models.BlockTypeLoop, "Loop.ForEach", 0),
			),
		})

		hashes := ComputeSubflowHashes(doc)
		if len(hashes) != 2 {
			t.Fatalf("expected 2 hashes, got %d", len(hashes))
		}
		if hashes[0].Hash == "" || hashes[1].Hash == "" {
			t.Error("hashes should not be empty")
		}
		if hashes[0].Hash == hashes[1].Hash {
			t.Error("different subflows should produce different hashes")
		}
	})

	t.Run("same_content_same_hash", func(t *testing.T) {
		doc := makeFlowDoc("f1", "Test", []models.Subflow{
			makeSubflow("sf1", "Main",
				makeBlock("b1", "Action", models.BlockTypeAction, "Action.Invoke", 0),
			),
		})

		h1 := ComputeSubflowHashes(doc)
		h2 := ComputeSubflowHashes(doc)
		if h1[0].Hash != h2[0].Hash {
			t.Errorf("same content should produce same hash: %s != %s", h1[0].Hash, h2[0].Hash)
		}
	})
}

func TestComputeDashboard(t *testing.T) {
	t.Run("empty_reports", func(t *testing.T) {
		d := ComputeDashboard(nil)
		if d.TotalFlowsAnalyzed != 0 {
			t.Errorf("expected 0 flows, got %d", d.TotalFlowsAnalyzed)
		}
		if d.FindingsBySeverity == nil {
			t.Error("expected non-nil severity map")
		}
	})

	t.Run("aggregates_multiple_reports", func(t *testing.T) {
		reports := []*models.AnalysisReport{
			{
				FlowID: "f1",
				Findings: []models.Finding{
					{Severity: models.SeverityError, RuleID: "R1", Category: "Reliability"},
					{Severity: models.SeverityWarning, RuleID: "R2", Category: "Style"},
				},
				Metrics: &models.FlowMetrics{HealthScore: 60},
			},
			{
				FlowID: "f2",
				Findings: []models.Finding{
					{Severity: models.SeverityError, RuleID: "R1", Category: "Security"},
				},
				Metrics: &models.FlowMetrics{HealthScore: 80},
			},
		}

		d := ComputeDashboard(reports)
		if d.TotalFlowsAnalyzed != 2 {
			t.Errorf("expected 2 flows, got %d", d.TotalFlowsAnalyzed)
		}
		if d.TotalFindings != 3 {
			t.Errorf("expected 3 findings, got %d", d.TotalFindings)
		}
		if d.FindingsBySeverity["error"] != 2 {
			t.Errorf("expected 2 errors, got %d", d.FindingsBySeverity["error"])
		}
		if d.FindingsByRule["R1"] != 2 {
			t.Errorf("expected 2 R1 findings, got %d", d.FindingsByRule["R1"])
		}
		if d.AvgHealthScore != 70.0 {
			t.Errorf("expected avg health 70, got %.1f", d.AvgHealthScore)
		}
		if len(d.TopProblemFlows) != 2 {
			t.Errorf("expected 2 problem flows, got %d", len(d.TopProblemFlows))
		}
	})
}

// TestFnvHasher_ByteSemantics guards L2: FNV-1a must hash the bytes of the
// string, not its runes. A rune-iterating implementation would produce the same
// hash for "é" (U+00E9) and the (impossible) single-byte string 0xE9, and a
// *different* hash from canonical FNV-1a-of-UTF-8-bytes. We verify against the
// well-known FNV-1a 32-bit reference: iterate the UTF-8 byte sequence.
func TestFnvHasher_ByteSemantics(t *testing.T) {
	// "é" is U+00E9, encoded in UTF-8 as the two bytes 0xC3 0xA9.
	h := fnvBuilder()
	h.write("é")
	got := h.sum()

	// Reference FNV-1a 32-bit over the bytes 0xC3 0xA9.
	var ref uint32 = 2166136261
	for _, b := range []byte("é") {
		ref ^= uint32(b)
		ref *= 16777619
	}
	want := fmt.Sprintf("%08x", ref)

	if got != want {
		t.Fatalf("fnvHasher on UTF-8: got %s, want canonical FNV-1a %s", got, want)
	}

	// Distinct byte sequences that share a rune code-point must NOT collide.
	// A rune-iterating impl hashes uint32(0xE9) for "é" and the same for a
	// hypothetical 1-byte 0xE9; the byte-iterating impl must differ.
	h2 := fnvBuilder()
	h2.write("\xe9") // single raw byte 0xE9 (invalid UTF-8, but a distinct byte string)
	if h2.sum() == got {
		t.Fatalf("collision: byte-iterating FNV-1a of \"\\xc3\\xa9\" == \"\\xe9\" (%s); expected distinct", got)
	}
}
