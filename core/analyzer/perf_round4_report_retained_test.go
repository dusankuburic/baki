package analyzer

import (
	"runtime"
	"testing"
)

// liveHeapBytes is the heap in use after a full collection.
func liveHeapBytes() int64 {
	runtime.GC()
	runtime.GC()
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	return int64(ms.HeapAlloc)
}

// TestAnalysisReport_RetainedBytesPerFinding pins what a cached report keeps
// resident. The engine used to materialize per-block finding groups on every
// report — each group repeated its findings plus a Primary copy — which was
// 44% of the report (0.41 KiB per finding) with no reader anywhere: an
// AnalysisReport cost ~0.94 KiB per finding (85 MiB for a finding-dense
// 10 MiB flow). Without the groups the realistic rule mix retains ~0.43 KiB
// per finding; the bar is 0.6 KiB so a re-materialized group set (or any
// other per-finding copy) fails here.
func TestAnalysisReport_RetainedBytesPerFinding(t *testing.T) {
	doc := buildLargeFlow(20000)
	rules := AllRules()

	before := liveHeapBytes()
	report := RunAnalysis(doc, rules, nil, nil)
	after := liveHeapBytes()

	n := len(report.Findings)
	if n < 10000 {
		t.Fatalf("synthetic flow produced %d findings; want a finding-dense report (>= 10000)", n)
	}
	retained := after - before
	perFinding := float64(retained) / float64(n)
	t.Logf("report with %d findings retains %d B (%.0f B per finding)", n, retained, perFinding)
	if perFinding > 614 {
		t.Fatalf("report retains %.0f B per finding, want <= 614 (0.6 KiB): per-finding copies (the per-block groups?) are being materialized again", perFinding)
	}
	// A finding is a ~216 B struct plus its own strings; a reading below that
	// means the report was not kept alive across the measurement.
	if perFinding < 128 {
		t.Fatalf("report retains only %.0f B per finding; the measurement is not seeing the report", perFinding)
	}
	runtime.KeepAlive(report)
	runtime.KeepAlive(doc)
}
