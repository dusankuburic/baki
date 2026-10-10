package analyzer

import (
	"context"
	"os"
	"strings"
	"testing"

	"pad-core/models"
	"pad-core/parser"
)

// Walk benchmarks on the parsed 9k-block sample: the whole rule set, the
// custom-rule tester's single rule (most facts unused), and the suppression
// inventory (a second full rule pass).
const benchSample10k = "../parser/testdata/samples/synthetic_10k.txt"

func loadBenchSample(b *testing.B, directive string) *models.FlowDocument {
	b.Helper()
	data, err := os.ReadFile(benchSample10k)
	if err != nil {
		b.Fatal(err)
	}
	src := strings.ReplaceAll(string(data), "\r\n", "\n")
	if directive != "" {
		src = strings.Replace(src, "#Region \"Main\"\n", "#Region \"Main\"\n    "+directive+"\n", 1)
	}
	doc, err := parser.ParseText(src, "synthetic_10k.txt", int64(len(src)))
	if err != nil {
		b.Fatal(err)
	}
	return doc
}

func BenchmarkRunAnalysisSample10k(b *testing.B) {
	doc := loadBenchSample(b, "")
	rules, settings := AllRules(), models.DefaultSettings()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = RunAnalysisCtxNoProfile(context.Background(), doc, rules, settings, nil)
	}
}

func BenchmarkRunAnalysisSingleRule(b *testing.B) {
	doc := loadBenchSample(b, "")
	rule, err := NewCustomRule(CustomRuleConfig{
		ID: "bench-message-box", Name: "Message box", Severity: "warning",
		RawTypeMatch: `^Display\.ShowMessageBox`,
	})
	if err != nil {
		b.Fatal(err)
	}
	rules := []Rule{rule}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = RunAnalysisCtx(context.Background(), doc, rules, nil, nil)
	}
}

func BenchmarkSuppressionInventory(b *testing.B) {
	doc := loadBenchSample(b, "# pad-ignore[unhandled-error]")
	rules, settings := AllRules(), models.DefaultSettings()
	if len(SuppressionInventory(doc, rules, settings)) == 0 {
		b.Fatal("no directive found")
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = SuppressionInventory(doc, rules, settings)
	}
}
