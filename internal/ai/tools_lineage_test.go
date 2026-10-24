package ai

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"pad-core/analyzer"
	"pad-core/models"
)

// docLineage is the analysis the tools see: the real lineage walk over the tool context's document.
type docLineage struct{}

func (docLineage) AnalyzeFlow(context.Context, *models.FlowDocument) (*models.AnalysisReport, error) {
	return nil, fmt.Errorf("not in this test")
}

func (docLineage) GetVariableLineage(doc *models.FlowDocument, name string) (*models.VariableHistory, error) {
	return analyzer.BuildVariableLineage(doc, name), nil
}

// lineageDoc: Result is set on line 1 and read on line 2; ResultCount and Total are other variables.
func lineageDoc() *models.FlowDocument {
	blocks := []models.Block{
		{ID: "b1", Name: "SET Result", Type: models.BlockTypeAction, RawType: "SET", LineNumber: 1, Properties: map[string]string{"_output": "Result"}},
		{ID: "b2", Name: "Show", Type: models.BlockTypeAction, RawType: "Display.ShowMessageBox", LineNumber: 2, Variables: []string{"Result"}},
		{ID: "b3", Name: "SET ResultCount", Type: models.BlockTypeAction, RawType: "SET", LineNumber: 3, Properties: map[string]string{"_output": "ResultCount"}},
		{ID: "b4", Name: "SET Total", Type: models.BlockTypeAction, RawType: "SET", LineNumber: 4, Properties: map[string]string{"_output": "Total"}, Variables: []string{"ResultCount"}},
	}
	doc := &models.FlowDocument{ID: "d", Name: "Lineage", Subflows: []models.Subflow{{ID: "sf", Name: "Main", Blocks: blocks}}}
	doc.RebuildIndexes()
	return doc
}

func lineage(t *testing.T, name string) string {
	t.Helper()
	in, _ := json.Marshal(map[string]string{"name": name})
	return ExecuteTool("get_variable_lineage", in, ToolContext{Ctx: context.Background(), Doc: lineageDoc(), Analysis: docLineage{}})
}

// get_variable_lineage passed its input through unchanged and the walk compares bare identifiers, so the
// %Result% spelling the model reads in property values and source files found nothing, and so did a different
// case; a miss gave no hint (R-analyzer-boundary-4).
func TestGetVariableLineage_NormalisesTheName(t *testing.T) {
	bare := lineage(t, "Result")
	if !strings.Contains(bare, `Lineage for "Result" (2 event(s))`) {
		t.Fatalf("the bare name finds nothing:\n%s", bare)
	}
	for _, spelling := range []string{"%Result%", " %Result% ", "Result "} {
		if got := lineage(t, spelling); got != bare {
			t.Errorf("%q answers differently from the bare name:\n%s\n--- want\n%s", spelling, got, bare)
		}
	}
	lower := lineage(t, "result")
	if !strings.Contains(lower, `Lineage for "Result" (2 event(s))`) {
		t.Errorf("a different case does not reach the variable through the case-insensitive fallback:\n%s", lower)
	}
}

func TestGetVariableLineage_MissListsCandidates(t *testing.T) {
	miss := lineage(t, "Res")
	if !strings.Contains(miss, `No lineage found for variable "Res"`) {
		t.Fatalf("not a miss:\n%s", miss)
	}
	for _, want := range []string{"Result", "ResultCount"} {
		if !strings.Contains(miss, want) {
			t.Errorf("the miss does not offer %q:\n%s", want, miss)
		}
	}
	if strings.Contains(miss, "Total") {
		t.Errorf("Total is no near match of Res:\n%s", miss)
	}
	none := lineage(t, "Nothing")
	if strings.Contains(none, "Did you mean") {
		t.Errorf("a miss with no near match offers names:\n%s", none)
	}
}

// Two variables that differ only in case are no fallback (which would guess) but both candidates.
func TestGetVariableLineage_AmbiguousCaseOffersBoth(t *testing.T) {
	blocks := []models.Block{
		{ID: "b1", Type: models.BlockTypeAction, RawType: "SET", LineNumber: 1, Properties: map[string]string{"_output": "Result"}},
		{ID: "b2", Type: models.BlockTypeAction, RawType: "SET", LineNumber: 2, Properties: map[string]string{"_output": "RESULT"}},
	}
	doc := &models.FlowDocument{ID: "d", Subflows: []models.Subflow{{ID: "sf", Name: "Main", Blocks: blocks}}}
	doc.RebuildIndexes()
	in, _ := json.Marshal(map[string]string{"name": "result"})
	out := ExecuteTool("get_variable_lineage", in, ToolContext{Ctx: context.Background(), Doc: doc, Analysis: docLineage{}})
	if !strings.Contains(out, "No lineage found") || !strings.Contains(out, "Did you mean: RESULT, Result?") {
		t.Errorf("an ambiguous case should be a miss naming both variables:\n%s", out)
	}
}

// At most five candidates, the first five by name.
func TestGetVariableLineage_AtMostFiveCandidates(t *testing.T) {
	var blocks []models.Block
	for i := 0; i < 9; i++ {
		blocks = append(blocks, models.Block{ID: fmt.Sprintf("b%d", i), Type: models.BlockTypeAction, RawType: "SET", LineNumber: i + 1,
			Properties: map[string]string{"_output": fmt.Sprintf("Item%d", i)}})
	}
	doc := &models.FlowDocument{ID: "d", Subflows: []models.Subflow{{ID: "sf", Name: "Main", Blocks: blocks}}}
	doc.RebuildIndexes()
	in, _ := json.Marshal(map[string]string{"name": "item"})
	out := ExecuteTool("get_variable_lineage", in, ToolContext{Ctx: context.Background(), Doc: doc, Analysis: docLineage{}})
	if !strings.HasSuffix(strings.TrimSpace(out), "Did you mean: Item0, Item1, Item2, Item3, Item4?") {
		t.Errorf("want the first five candidates by name:\n%s", out)
	}
}
