package parser

import (
	"strings"
	"testing"
	"time"

	"pad-core/models"
)

// linearScanBudget is a deliberately generous wall-clock bound. The point is not
// to measure performance — it is to distinguish O(n) from O(n²), which differ
// here by four orders of magnitude, so any threshold in this range separates
// them without flaking on a loaded machine.
const linearScanBudget = 2 * time.Second

// scanSize is small enough that the linear version is instant and large enough
// that the quadratic version cannot finish inside the budget. At 200 KB the
// pre-fix scanners performed ~2×10¹⁰ byte comparisons; a 2 MB line (the actual
// limit on /api/analysis/analyze-raw) was ~2×10¹² — tens of minutes of pinned
// CPU that no request timeout could interrupt, because the parser takes no
// context. Using 200 KB keeps this test fast once fixed while still failing
// unmistakably if the rewind ever returns.
const scanSize = 200 * 1024

// TestParseProperties_UnterminatedKeyRunIsLinear is the regression gate for the
// quadratic rewind in findNextKey.
//
// A run of key characters that is never followed by ": " used to be rescanned
// from keyStart+1 on every failure, so an N-byte run cost N²/2 comparisons. No
// key can begin inside a maximal key-char run — every sub-run ends at the same
// terminator, so it fails the same test — which is why skipping the whole run is
// behaviour-preserving.
func TestParseProperties_UnterminatedKeyRunIsLinear(t *testing.T) {
	// The trailing "Key: V" is load-bearing, not decoration: parseProperties
	// short-circuits on a line with no ':' anywhere, so an input consisting only
	// of the long run never reaches the scanner and the test would pass without
	// exercising anything. The real key must also still be found afterwards,
	// which is what proves skipping the run did not skip past it.
	line := "Excel.ReadCell " + strings.Repeat("a", scanSize) + " Key: V"

	done := make(chan map[string]string, 1)
	go func() { done <- parseProperties(line) }()

	select {
	case props := <-done:
		if got := props["Key"]; got != "V" {
			t.Errorf("key after the long run = %q, want %q (all: %v)", got, "V", props)
		}
	case <-time.After(linearScanBudget):
		t.Fatalf("parseProperties did not finish within %v on a %d-byte key run — "+
			"the scanner is rescanning instead of skipping it", linearScanBudget, scanSize)
	}
}

// TestParseProperties_LongValueIsLinear is the same gate for findValueEnd, which
// had the identical shape: it peeked to the end of each key-char run to test for
// a "key: " boundary, then advanced by a single byte and peeked again.
func TestParseProperties_LongValueIsLinear(t *testing.T) {
	line := "Excel.ReadCell Key: " + strings.Repeat("b", scanSize)

	done := make(chan map[string]string, 1)
	go func() { done <- parseProperties(line) }()

	select {
	case props := <-done:
		if got := props["Key"]; len(got) != scanSize {
			t.Errorf("value length = %d, want %d (the whole run is the value)", len(got), scanSize)
		}
	case <-time.After(linearScanBudget):
		t.Fatalf("parseProperties did not finish within %v on a %d-byte value — "+
			"findValueEnd is rescanning instead of skipping", linearScanBudget, scanSize)
	}
}

// TestParseProperties_ColonWithoutSpaceIsLinear covers the second rewind in
// findNextKey: a run IS followed by ':' but the next byte is not a space or tab,
// so it is not a key boundary. Repeating that shape used to rewind every time.
func TestParseProperties_ColonWithoutSpaceIsLinear(t *testing.T) {
	// "aaa...a:x" — a colon that never begins a real key/value pair.
	line := "Excel.ReadCell " + strings.Repeat("a", scanSize) + ":x"

	done := make(chan map[string]string, 1)
	go func() { done <- parseProperties(line) }()

	select {
	case <-done:
	case <-time.After(linearScanBudget):
		t.Fatalf("parseProperties did not finish within %v on a %d-byte colon-terminated run",
			linearScanBudget, scanSize)
	}
}

// TestParseProperties_SkippingRunsPreservesParsing is the correctness half: the
// optimisation must not cause a real key to be missed. Each case places a valid
// key immediately after a construct the scanner now skips wholesale.
func TestParseProperties_SkippingRunsPreservesParsing(t *testing.T) {
	cases := []struct {
		name string
		line string
		want map[string]string
	}{
		{
			name: "key after an unterminated run",
			line: "Excel.ReadCell junkjunkjunk Key: Value",
			want: map[string]string{"Key": "Value"},
		},
		{
			name: "key after a colon with no space",
			line: "Excel.ReadCell ratio:5 Key: Value",
			want: map[string]string{"Key": "Value"},
		},
		{
			name: "two keys in sequence",
			line: "Excel.ReadCell First: one Second: two",
			want: map[string]string{"First": "one", "Second": "two"},
		},
		{
			name: "dotted key survives (isKeyChar includes '.')",
			line: "Excel.ReadCell Instance.Name: sheet1",
			want: map[string]string{"Instance.Name": "sheet1"},
		},
		{
			name: "value containing a colon that is not a boundary",
			line: "Web.Go Url: https://example.com/x",
			want: map[string]string{"Url": "https://example.com/x"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseProperties(tc.line)
			for k, want := range tc.want {
				if got[k] != want {
					t.Errorf("property %q = %q, want %q (all: %v)", k, got[k], want, got)
				}
			}
		})
	}
}

// TestParser_NestedErrorHandlersRespectDepthLimit pins the depth guard on
// ON BLOCK ERROR.
//
// It was the only block-opening handler that did not call requireDepth, so
// nested error handlers were the one construct that could push past
// maxNestingDepth. Every recursive walker downstream (toValueTree,
// CountDescendants, computeMaxDepth, the search indexer, the serializer, the
// analyzer) then runs at whatever depth the input chose.
func TestParser_NestedErrorHandlersRespectDepthLimit(t *testing.T) {
	const depth = maxNestingDepth + 50

	var sb strings.Builder
	sb.WriteString("#Region \"Main\"\n")
	for i := 0; i < depth; i++ {
		sb.WriteString(strings.Repeat("    ", i+1))
		sb.WriteString("ON BLOCK ERROR\n")
	}
	sb.WriteString("#EndRegion\n")
	src := sb.String()

	doc, err := ParseText(src, "Main.txt", int64(len(src)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}

	var depthErr bool
	for _, pe := range doc.ParseErrors {
		if strings.Contains(pe.Message, "nesting depth exceeds") {
			depthErr = true
			break
		}
	}
	if !depthErr {
		// Deliberately NOT dumping doc.ParseErrors: this input produces one
		// "unclosed block" error per line, so printing them buries the actual
		// failure under 250 near-identical lines.
		t.Errorf("%d nested ON BLOCK ERROR produced no depth error (%d parse errors, first: %q)",
			depth, len(doc.ParseErrors), firstParseErrMsg(doc.ParseErrors))
	}

	// And the tree must actually be capped, not merely complained about.
	var deepest func(blocks []models.Block, d int) int
	deepest = func(blocks []models.Block, d int) int {
		max := d
		for i := range blocks {
			if got := deepest(blocks[i].Children, d+1); got > max {
				max = got
			}
		}
		return max
	}
	got := 0
	for i := range doc.Subflows {
		if d := deepest(doc.Subflows[i].Blocks, 1); d > got {
			got = d
		}
	}
	if got > maxNestingDepth+1 {
		t.Errorf("tree depth = %d, want <= %d — the guard reported but did not stop the push",
			got, maxNestingDepth+1)
	}
}

func firstParseErrMsg(errs []models.ParseError) string {
	if len(errs) == 0 {
		return "<none>"
	}
	return errs[0].Message
}
