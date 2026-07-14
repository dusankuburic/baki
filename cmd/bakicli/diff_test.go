package main

import (
	"bytes"
	"io"
	"os"
	"strings"
	"testing"

	"pad-core/analyzer"
	"pad-core/models"
	"pad-core/parser"
)

// parseHelper parses a PAD source string into a FlowDocument for tests.
func parseHelper(t *testing.T, source string) *models.FlowDocument {
	t.Helper()
	doc, err := parser.ParseText(source, "Main.txt", int64(len(source)))
	if err != nil {
		t.Fatalf("parse: %v\nsource:\n%s", err, source)
	}
	return doc
}

// TestDiffFlows_TwoFiles verifies the structural diff the CLI's `diff` command
// consumes: an added block and a removed block surface with the right change
// kind. Uses distinct action types so the LCS matcher (which ignores property
// values) sees them as add/remove rather than same-block-modified.
func TestDiffFlows_TwoFiles(t *testing.T) {
	oldDoc := parseHelper(t, "#Region \"Main\"\nDisplay.ShowMessageBox Message: 'keep'\nText.AppendLine Text: 'gone'\n#EndRegion\n")
	newDoc := parseHelper(t, "#Region \"Main\"\nDisplay.ShowMessageBox Message: 'keep'\nFile.WriteText Content: 'new'\n#EndRegion\n")

	diff := parser.DiffFlows(oldDoc, newDoc)
	if len(diff.Subflows) == 0 {
		t.Fatal("expected at least one subflow diff")
	}
	var added, removed int
	for _, b := range diff.Subflows[0].Blocks {
		switch b.Change {
		case models.ChangeAdded:
			added++
		case models.ChangeRemoved:
			removed++
		}
	}
	if added != 1 {
		t.Errorf("expected 1 added block, got %d", added)
	}
	if removed != 1 {
		t.Errorf("expected 1 removed block, got %d", removed)
	}
}

// TestCompareFlows_TwoFiles verifies the semantic comparison the CLI's
// `diff --semantic` consumes.
func TestCompareFlows_TwoFiles(t *testing.T) {
	oldDoc := parseHelper(t, "#Region \"Main\"\nDisplay.ShowMessageBox Message: 'keep'\nText.AppendLine Text: 'gone'\n#EndRegion\n")
	newDoc := parseHelper(t, "#Region \"Main\"\nDisplay.ShowMessageBox Message: 'keep'\nFile.WriteText Content: 'new'\n#EndRegion\n")

	comp := analyzer.CompareFlows(oldDoc, newDoc)
	if comp.AddedBlocks != 1 {
		t.Errorf("expected 1 added block, got %d", comp.AddedBlocks)
	}
	if comp.RemovedBlocks != 1 {
		t.Errorf("expected 1 removed block, got %d", comp.RemovedBlocks)
	}
}

// captureStdout runs f with os.Stdout redirected to a pipe and returns what it
// printed. Tests are sequential (no t.Parallel), so the global swap is safe.
func captureStdout(f func()) string {
	old := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w
	f()
	w.Close()
	os.Stdout = old
	var buf bytes.Buffer
	_, _ = io.Copy(&buf, r)
	return buf.String()
}

// TestPrintDiffText_ChangesOnly verifies the CLI text formatter renders +/- for
// changed blocks and skips unchanged ones.
func TestPrintDiffText_ChangesOnly(t *testing.T) {
	oldDoc := parseHelper(t, "#Region \"Main\"\nDisplay.ShowMessageBox Message: 'keep'\nText.AppendLine Text: 'gone'\n#EndRegion\n")
	newDoc := parseHelper(t, "#Region \"Main\"\nDisplay.ShowMessageBox Message: 'keep'\nFile.WriteText Content: 'new'\n#EndRegion\n")
	diff := parser.DiffFlows(oldDoc, newDoc)

	out := captureStdout(func() { printDiffText(diff, "old.txt", "new.txt") })
	if !strings.Contains(out, "diff old.txt → new.txt") {
		t.Errorf("expected header, got:\n%s", out)
	}
	if !strings.Contains(out, "- Append Line") {
		t.Errorf("expected '- Append Line' (removed Text.AppendLine), got:\n%s", out)
	}
	if !strings.Contains(out, "+ Write Text") {
		t.Errorf("expected '+ Write Text' (added File.WriteText), got:\n%s", out)
	}
	if strings.Contains(out, "Show Message Box") {
		t.Errorf("unchanged block 'Show Message Box' should be filtered out, got:\n%s", out)
	}
}
