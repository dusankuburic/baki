package analyzer

import (
	"context"
	"fmt"
	"math/rand"
	"os"
	"testing"

	"pad-core/models"
)

// libGenerateFlows builds n path-backed flows of 4-12 subflows each (3-30
// actions, every seventh a LOOP with two children); about 30% of the subflows
// copy one of 150 shared bodies under their own name, the rest are unique.
func libGenerateFlows(n int, seed int64) []*models.FlowDocument {
	rng := rand.New(rand.NewSource(seed))
	rawTypes := []string{"Variables.SetVariable", "Display.ShowMessageBox", "WebAutomation.Click.Click",
		"File.ReadTextFromFile", "Excel.WriteToExcel"}
	body := func(tag string, size int) []models.Block {
		blocks := make([]models.Block, 0, size)
		for k := range size {
			rt := rawTypes[rng.Intn(len(rawTypes))]
			b := models.Block{Name: fmt.Sprintf("%s %s %d", rt, tag, k), Type: models.BlockTypeAction, RawType: rt,
				Properties: map[string]string{"Value": fmt.Sprintf("%s-%d", tag, rng.Intn(1000)), "Name": "Var"}}
			if k%7 == 6 {
				b.Type, b.RawType, b.Name = models.BlockTypeLoop, "LOOP", "Loop "+tag
				b.Children = []models.Block{
					{Name: "inner", Type: models.BlockTypeAction, RawType: rt, Properties: map[string]string{"Value": tag}},
					{Name: "End", Type: models.BlockTypeEnd, RawType: "END"},
				}
			}
			blocks = append(blocks, b)
		}
		return blocks
	}
	templates := make([][]models.Block, 150)
	for i := range templates {
		templates[i] = body(fmt.Sprintf("t%d", i), 3+rng.Intn(28))
	}
	nextID := 0
	var stamp func(blocks []models.Block, sfID string) []models.Block
	stamp = func(blocks []models.Block, sfID string) []models.Block {
		out := make([]models.Block, len(blocks))
		for i, b := range blocks {
			nextID++
			b.ID, b.SubflowID, b.LineNumber = fmt.Sprintf("b%d", nextID), sfID, nextID
			b.Children = stamp(b.Children, sfID)
			out[i] = b
		}
		return out
	}
	docs := make([]*models.FlowDocument, n)
	for i := range docs {
		doc := &models.FlowDocument{ID: fmt.Sprintf("doc-%04d", i), Name: fmt.Sprintf("Flow %04d", i),
			FilePath: fmt.Sprintf("library/flow_%04d.txt", i)}
		for j := range 4 + rng.Intn(9) {
			sfID := fmt.Sprintf("sf-%d-%d", i, j)
			var blocks []models.Block
			if rng.Intn(10) < 3 {
				blocks = templates[rng.Intn(len(templates))]
			} else {
				blocks = body(fmt.Sprintf("f%d-s%d", i, j), 3+rng.Intn(28))
			}
			doc.Subflows = append(doc.Subflows, models.Subflow{ID: sfID, Name: fmt.Sprintf("Sub%d", j), Blocks: stamp(blocks, sfID)})
		}
		doc.RebuildIndexes()
		docs[i] = doc
	}
	return docs
}

func TestLibGenerateFlows_HasCrossFlowDuplicates(t *testing.T) {
	docs := libGenerateFlows(200, 1)
	rep := AnalyzeLibrary(context.Background(), docs, LibraryRules(), nil)
	if len(rep.Findings) < 50 || rep.Flows != 200 || rep.Truncated {
		t.Errorf("generator: %d findings over %d flows (truncated %v)", len(rep.Findings), rep.Flows, rep.Truncated)
	}
}

// BenchmarkLibraryExtract_Synthetic10k: the per-flow extraction cost on the
// 9,149-block sample, split into its parts, against a full analysis.
func BenchmarkLibraryExtract_Synthetic10k(b *testing.B) {
	data, err := os.ReadFile("../parser/testdata/samples/synthetic_10k.txt")
	if err != nil {
		b.Skip("synthetic_10k.txt not found")
	}
	doc := libParse(b, "samples/synthetic_10k.txt", string(data))
	rules := LibraryRules()
	ctx := context.Background()
	b.Run("summary", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := ExtractFlowSummary(ctx, doc, nil, rules); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("context", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			libraryRuleContext(doc, nil)
		}
	})
	rctx := libraryRuleContext(doc, nil)
	b.Run("rule-extract", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			if _, err := (&DuplicateSubflowAcrossFlowsRule{}).Extract(rctx); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("analysis", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			RunAnalysisCtxNoProfile(ctx, doc, AllRules(), nil, nil)
		}
	})
}

// BenchmarkLibraryDuplicateSubflow_1000Flows: the library pass over 1000
// generated flows: extraction alone, the check over extracted summaries, and
// AnalyzeLibrary (parallel extraction plus check).
func BenchmarkLibraryDuplicateSubflow_1000Flows(b *testing.B) {
	docs := libGenerateFlows(1000, 42)
	rules := LibraryRules()
	ctx := context.Background()
	sums := make([]models.FlowSummary, len(docs))
	factBytes := 0
	for i, d := range docs {
		s, err := ExtractFlowSummary(ctx, d, nil, rules)
		if err != nil {
			b.Fatal(err)
		}
		sums[i] = s
		for _, f := range s.Facts {
			factBytes += len(f.Data)
		}
	}
	lib := NewMemoryLibrary(sums)
	rep := RunLibraryRules(ctx, rules, lib, nil)
	b.Logf("1000 flows: %d facts bytes, %d findings", factBytes, len(rep.Findings))
	b.Run("extract", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			for _, d := range docs {
				if _, err := ExtractFlowSummary(ctx, d, nil, rules); err != nil {
					b.Fatal(err)
				}
			}
		}
	})
	b.Run("check", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			RunLibraryRules(ctx, rules, lib, nil)
		}
	})
	b.Run("analyze-library", func(b *testing.B) {
		b.ReportAllocs()
		for b.Loop() {
			AnalyzeLibrary(ctx, docs, rules, nil)
		}
	})
}
