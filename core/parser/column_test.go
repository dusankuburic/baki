package parser

import (
	"strings"
	"testing"
)

// TestParseErrorColumn_Populated is the gate for ParseError.Column: the
// tokenizer now tracks the 1-based column of a line's first non-indent byte,
// and every token-scoped parse error must carry it (0 only for errors with no
// natural column, e.g. unclosed blocks at a subflow boundary).
func TestParseErrorColumn_Populated(t *testing.T) {
	// ELSE outside IF, indented 8 columns (2 levels × 4 spaces).
	src := "#Region \"Main\"\n" +
		"LOOP\n" +
		"        ELSE\n" +
		"END\n" +
		"#EndRegion\n"
	doc, err := ParseText(src, "Main.txt", int64(len(src)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	found := false
	for _, pe := range doc.ParseErrors {
		if strings.Contains(pe.Message, "ELSE outside IF") {
			found = true
			if pe.Line != 3 {
				t.Errorf("ELSE error line = %d, want 3", pe.Line)
			}
			if pe.Column != 9 {
				t.Errorf("ELSE error column = %d, want 9 (8 spaces of indent + 1)", pe.Column)
			}
		}
	}
	if !found {
		t.Fatalf("expected 'ELSE outside IF' parse error, got %+v", doc.ParseErrors)
	}
}

// TestTokenColumnZeroForUnindented confirms column 1 (not 0) for a top-level
// error — 0 is reserved for "unknown/no natural column".
func TestTokenColumnZeroForUnindented(t *testing.T) {
	src := "#Region \"Main\"\nCASE 'x'\n#EndRegion\n"
	doc, err := ParseText(src, "Main.txt", int64(len(src)))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, pe := range doc.ParseErrors {
		if strings.Contains(pe.Message, "CASE outside SWITCH") {
			if pe.Column != 1 {
				t.Errorf("top-level CASE error column = %d, want 1", pe.Column)
			}
			return
		}
	}
	t.Fatalf("expected 'CASE outside SWITCH' parse error, got %+v", doc.ParseErrors)
}
