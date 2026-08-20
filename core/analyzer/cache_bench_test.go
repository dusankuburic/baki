package analyzer

import (
	"fmt"
	"testing"

	"pad-core/models"
)

// BenchmarkFlowHash measures the analysis-cache key computation on a large
// flow. FlowHash walks every block and sorts every block's property keys, and
// it runs on every CachedAnalysis call — including cache hits — so a
// regression here taxes every analysis request even when fully cached.
func BenchmarkFlowHash(b *testing.B) {
	flow := buildLargeFlow(8000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = FlowHash(flow)
	}
}

// makeFindings builds n findings with a mix of duplicate shapes: groups of 3
// share (BlockID, Title, subject), the rest are unique — the shape
// DeduplicateFindings groups per block on every report.
func makeFindings(n int) []models.Finding {
	findings := make([]models.Finding, 0, n)
	for i := 0; i < n; i++ {
		f := models.Finding{
			RuleID:  fmt.Sprintf("rule-%d", i%40),
			Title:   fmt.Sprintf("Title %d", i/3),
			BlockID: fmt.Sprintf("block-%d", i/3),
		}
		if i%3 == 0 {
			f.Metadata = map[string]interface{}{"variable": fmt.Sprintf("v%d", i/3)}
		}
		findings = append(findings, f)
	}
	return findings
}

// BenchmarkDeduplicateFindings guards the dedup grouping pass (runs on every
// report before stats/IDs/fingerprints are derived).
func BenchmarkDeduplicateFindings(b *testing.B) {
	findings := makeFindings(10000)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _ = DeduplicateFindings(findings)
	}
}
