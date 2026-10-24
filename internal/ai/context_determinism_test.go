package ai

import (
	"fmt"
	"strings"
	"testing"

	"pad-core/models"
)

// BuildContext ranged over four Go maps (a block finding's metadata, the selected block's properties, the
// Analysis Summary's per-title counts and the variable history), so two builds of one request could differ byte
// for byte: the final cut then kept different lines from one rebuild to the next, the preview listed them in
// another order than the send, and no golden could pin the assembled request (R-streaming-9, R-testing-8). The
// request below has 5 metadata keys, 4 properties, 6 finding titles and 3 variables; two dozen builds must agree.
func TestBuildContext_Deterministic(t *testing.T) {
	block := &models.Block{
		ID: "b1", Name: "Set creds", Type: models.BlockTypeAction, RawType: "Variables.SetVariable", LineNumber: 4, SubflowID: "sf1",
		Properties: map[string]string{"Name": "Creds", "NewValue": "x", "Scope": "flow", "Kind": "text"},
	}
	doc := makeMinimalDoc("Det")
	doc.BlocksByID["b1"] = block
	findings := []models.Finding{
		{BlockID: "b1", Severity: models.SeverityError, Title: "Hardcoded credential", Description: "d", Suggestion: "s",
			Metadata: map[string]interface{}{"variable": "Creds", "property": "NewValue", "line": 4, "confidence": "high", "source": "literal"}},
	}
	for i, title := range []string{"Infinite loop risk", "Unused variable", "Missing error handling", "Slow loop", "Hardcoded path"} {
		for j := 0; j <= i%3; j++ {
			findings = append(findings, models.Finding{BlockID: fmt.Sprintf("x%d-%d", i, j), Severity: models.SeverityWarning, Title: title, Description: "d"})
		}
	}
	events := map[string][]models.VariableEvent{
		"Creds":  {{Type: "init", BlockID: "b1", Line: 4}},
		"Result": {{Type: "init", BlockID: "b2", Line: 9}, {Type: "read", BlockID: "b3", Line: 12}},
		"Count":  {{Type: "mutate", BlockID: "b4", Line: 20}},
	}
	req := ContextRequest{Flow: doc, SelectedBlock: block, Findings: findings, VariableEvents: events, TokenBudget: 8000}

	firstSys, firstCtx := BuildContext(req)
	for i := 0; i < 24; i++ {
		sys, ctx := BuildContext(req)
		if sys != firstSys {
			t.Fatalf("build %d: the system prompt differs from the first build", i)
		}
		if ctx != firstCtx {
			t.Fatalf("build %d: the context differs from the first build:\n--- first\n%s\n--- this\n%s", i, firstCtx, ctx)
		}
	}
	for _, want := range []string{"## Analysis Summary", "**Properties:**", "## Known Issues with This Block", "## Variable History"} {
		if !strings.Contains(firstCtx, want) {
			t.Errorf("the fixture does not exercise %q:\n%s", want, firstCtx)
		}
	}
}

// The Analysis Summary lists titles by count, most frequent first, then by title: the most common problem
// leads, and the order no longer depends on map iteration.
func TestWriteFindingsSummary_OrderedByCountThenTitle(t *testing.T) {
	var findings []models.Finding
	add := func(title string, n int) {
		for i := 0; i < n; i++ {
			findings = append(findings, models.Finding{BlockID: fmt.Sprintf("%s-%d", title, i), Title: title, Severity: models.SeverityWarning})
		}
	}
	add("Zeta", 2)
	add("Alpha", 2)
	add("Mid", 3)
	add("Solo", 1)
	var b strings.Builder
	writeFindingsSummary(&b, findings)
	out := b.String()
	order := []string{"- Mid (3 occurrences)", "- Alpha (2 occurrences)", "- Zeta (2 occurrences)", "- Solo (1 occurrence)"}
	last := -1
	for _, line := range order {
		at := strings.Index(out, line)
		if at < 0 {
			t.Fatalf("summary lacks %q:\n%s", line, out)
		}
		if at < last {
			t.Errorf("%q is out of order (want count descending, then title):\n%s", line, out)
		}
		last = at
	}
}

// The selected block's properties and a finding's metadata are listed by key, so the same block reads the same
// way in every turn and in the preview.
func TestWriteBlockDetail_PropertiesSortedByKey(t *testing.T) {
	block := &models.Block{ID: "b1", Name: "n", Type: models.BlockTypeAction, RawType: "X.Y",
		Properties: map[string]string{"Zed": "1", "Alpha": "2", "Mid": "3", "Beta": "4"}}
	var b strings.Builder
	writeBlockDetail(&b, block)
	out := b.String()
	for i, k := range []string{"- Alpha:", "- Beta:", "- Mid:", "- Zed:"} {
		if at := strings.Index(out, k); at < 0 || (i > 0 && at < strings.Index(out, "- Alpha:")) {
			t.Errorf("properties are not sorted by key:\n%s", out)
			break
		}
	}
	if strings.Index(out, "- Alpha:") > strings.Index(out, "- Beta:") || strings.Index(out, "- Beta:") > strings.Index(out, "- Mid:") || strings.Index(out, "- Mid:") > strings.Index(out, "- Zed:") {
		t.Errorf("properties are not sorted by key:\n%s", out)
	}
}
