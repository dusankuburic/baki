package parser

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"pad-core/models"
)

// everySampleFlows walks the parser sample corpus (files + folder flows) and
// returns (name, content) pairs — the serializer's ground truth inputs.
func everySampleFlows(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	root := "testdata/samples"
	err := filepath.Walk(root, func(p string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() {
			return err
		}
		if strings.HasSuffix(p, ".txt") || strings.HasSuffix(p, ".pad") {
			data, rerr := os.ReadFile(p)
			if rerr != nil {
				return rerr
			}
			out[p] = string(data)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk samples: %v", err)
	}
	if len(out) < 10 {
		t.Fatalf("expected the full sample corpus, got %d files", len(out))
	}
	return out
}

// treeSig renders a normalized structural signature of a parsed document:
// block type, raw type, sorted non-internal properties, and child shape.
// IDs/indents/line numbers/tokens/names are excluded — exactly the fields
// canonical serialization is allowed to normalize.
func treeSig(doc *models.FlowDocument) string {
	var sb strings.Builder
	for i := range doc.Subflows {
		sf := &doc.Subflows[i]
		fmt.Fprintf(&sb, "SF %q{", sf.Name)
		sigBlocks(&sb, sf.Blocks)
		sb.WriteString("} ")
	}
	return sb.String()
}

func sigBlocks(sb *strings.Builder, blocks []models.Block) {
	for i := range blocks {
		b := &blocks[i]
		if b.Type == models.BlockTypeEnd {
			// END children are parse artifacts; containers regenerate them.
			continue
		}
		keys := make([]string, 0, len(b.Properties))
		for k := range b.Properties {
			if strings.HasPrefix(k, "_") && k != "_output" && k != "_var" && k != "_value" {
				continue // internal only (_parentType, _retry*)
			}
			keys = append(keys, k)
		}
		sort.Strings(keys)
		props := make([]string, 0, len(keys))
		for _, k := range keys {
			props = append(props, k+"="+b.Properties[k])
		}
		fmt.Fprintf(sb, "[%s|%s|%s(", b.Type, b.RawType, strings.Join(props, ","))
		// SET values keep raw quoting — normalize the wrapper for comparison
		// since re-emission is verbatim but the ORIGINAL may differ (e.g.
		// 'single' vs $'''triple''' quoting of the same value).
		sb.WriteString(")")
		sigBlocks(sb, b.Children)
		sb.WriteString("]")
	}
}

// normalizeSetValue strips quote wrappers from _value so original vs
// serialized trees compare equal regardless of quoting style.
func normalizeSetValue(b *models.Block) {
	if v, ok := b.Properties["_value"]; ok {
		b.Properties["_value"] = stripQuotes(v)
	}
	for i := range b.Children {
		normalizeSetValue(&b.Children[i])
	}
}

// TestSerializeRoundTrip_AllSamples is THE serializer gate: every sample
// flow parses → serializes → re-parses into an equivalent document with no
// NEW parse errors. This is the contract the fix/export paths rely on.
func TestSerializeRoundTrip_AllSamples(t *testing.T) {
	for name, src := range everySampleFlows(t) {
		t.Run(filepath.Base(name), func(t *testing.T) {
			orig, err := ParseText(src, filepath.Base(name), int64(len(src)))
			if err != nil {
				t.Fatalf("original parse: %v", err)
			}

			out := SerializeDocument(orig)
			round, err := ParseText(out, "round-"+filepath.Base(name), int64(len(out)))
			if err != nil {
				t.Fatalf("round-trip parse: %v\n--- serialized ---\n%s", err, out)
			}
			if got, want := len(round.ParseErrors), len(orig.ParseErrors); got > want {
				t.Fatalf("round-trip introduced parse errors (%d → %d):\n%v\n--- serialized ---\n%s",
					want, got, round.ParseErrors, out)
			}

			normalizeTree(orig)
			normalizeTree(round)
			if a, b := treeSig(orig), treeSig(round); a != b {
				t.Fatalf("tree diverged.\norig:  %s\nround: %s\n--- serialized ---\n%s", a, b, out)
			}
		})
	}
}

func normalizeTree(doc *models.FlowDocument) {
	for i := range doc.Subflows {
		for j := range doc.Subflows[i].Blocks {
			normalizeSetValue(&doc.Subflows[i].Blocks[j])
		}
	}
}

// TestSerializeFiles_GroupsBySourceFile: folder documents round-trip through
// the per-file map with SourceFile preserved.
func TestSerializeFiles_GroupsBySourceFile(t *testing.T) {
	files := map[string]string{
		"Main.txt":   "#Region \"Main\"\n    SET A TO 1\n#EndRegion\n",
		"Helper.pad": "#Region \"Helper\"\n    COMMENT  hi\n#EndRegion\n",
	}
	doc, err := ParseFiles(files, "proj")
	if err != nil {
		t.Fatalf("ParseFiles: %v", err)
	}
	out := SerializeFiles(doc)
	if len(out) != 2 {
		t.Fatalf("want 2 files, got %d: %v", len(out), out)
	}
	for name := range files {
		if _, ok := out[name]; !ok {
			t.Errorf("file %q missing from output: %v", name, out)
		}
	}
	// Each file re-parses cleanly and preserves its subflow name.
	for name, content := range out {
		round, err := ParseText(content, name, int64(len(content)))
		if err != nil || len(round.Subflows) == 0 {
			t.Errorf("%s re-parse failed: %v", name, err)
			continue
		}
	}
	if round, _ := ParseText(out["Main.txt"], "Main.txt", 0); round.Subflows[0].Name != "Main" {
		t.Errorf("subflow name lost: %q", round.Subflows[0].Name)
	}
}

// TestQuoteValue pins the value-quoting decision table.
func TestQuoteValue(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "''''''"},
		{"OK", "OK"}, // enum
		{"Display.Icon.None", "Display.Icon.None"},
		{"https://example.com", "https://example.com"},
		{"%Var%", "%Var%"},
		{"%Var['col']%", "$'''%Var['col']%'''"}, // quotes → quoted
		{"hello world", "$'''hello world'''"},   // space → quoted
		{"Line1\nLine2", "$'''Line1\nLine2'''"},
	}
	for _, tc := range cases {
		if got := QuoteValue(tc.in); got != tc.want {
			t.Errorf("QuoteValue(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestSerializeSubflow_RegionFraming: names with quotes/newlines can't break
// out of the region header.
func TestSerializeSubflow_RegionFraming(t *testing.T) {
	sf := &models.Subflow{Name: "Evil \"Name\"\nwith newline", Blocks: nil}
	out := SerializeSubflow(sf)
	if strings.Count(out, "\n") != 2 { // header + end marker only
		t.Errorf("multiline name corrupted region framing: %q", out)
	}
	round, err := ParseText(out, "x", int64(len(out)))
	if err != nil || len(round.Subflows) != 1 {
		t.Fatalf("framing re-parse failed: %v (%q)", err, out)
	}
	if round.Subflows[0].Name != `Evil 'Name' with newline` {
		t.Errorf("sanitized name = %q", round.Subflows[0].Name)
	}
}
