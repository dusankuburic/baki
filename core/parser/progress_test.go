package parser

import (
	"testing"
)

const samplePAD = `
SET MyVar TO 'hello'
File.OpenTextFile File: 'C:\data.txt' Encoding: File.TextFileEncoding.UTF8 ReadAs: File.FileReadAs.WholeText => FileContent
`

func TestParseTextWithProgress_NilCallback_DelegatesToParseText(t *testing.T) {
	doc, err := ParseTextWithProgress(samplePAD, "test.txt", int64(len(samplePAD)), nil)
	if err != nil {
		t.Fatalf("ParseTextWithProgress(nil callback): %v", err)
	}
	if doc == nil {
		t.Fatal("expected non-nil document")
	}

	// Must produce the same result as ParseText for the same input.
	docDirect, err := ParseText(samplePAD, "test.txt", int64(len(samplePAD)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}
	if doc.Metadata.BlockCount != docDirect.Metadata.BlockCount {
		t.Errorf("block count mismatch: progress=%d direct=%d", doc.Metadata.BlockCount, docDirect.Metadata.BlockCount)
	}
}

func TestParseTextWithProgress_SmallFile_DelegatesToParseText(t *testing.T) {
	// fileSize < 1_000_000 → delegates even with a non-nil callback.
	called := false
	doc, err := ParseTextWithProgress(samplePAD, "test.txt", 100, func(pct int, msg string) {
		called = true
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc == nil {
		t.Fatal("expected non-nil document")
	}
	if called {
		t.Error("callback should NOT be called for small files")
	}
}

func TestParseTextWithProgress_LargeFile_FiresProgressCallbacks(t *testing.T) {
	var percentages []int
	doc, err := ParseTextWithProgress(samplePAD, "test.txt", 2_000_000, func(pct int, msg string) {
		percentages = append(percentages, pct)
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc == nil {
		t.Fatal("expected non-nil document")
	}

	// Must fire at least the fixed checkpoints (10, 95, 100).
	if len(percentages) < 2 {
		t.Errorf("expected at least 2 progress callbacks, got %d", len(percentages))
	}
	// First callback is always 10 ("Tokenized").
	if percentages[0] != 10 {
		t.Errorf("first callback percent = %d, want 10", percentages[0])
	}
	// Last callback is always 100 ("Done").
	if last := percentages[len(percentages)-1]; last != 100 {
		t.Errorf("last callback percent = %d, want 100", last)
	}
}

func TestParseTextWithProgress_LargeFile_SameResultAsParseText(t *testing.T) {
	docProgress, err := ParseTextWithProgress(samplePAD, "test.txt", 2_000_000, func(int, string) {})
	if err != nil {
		t.Fatalf("ParseTextWithProgress: %v", err)
	}
	docDirect, err := ParseText(samplePAD, "test.txt", int64(len(samplePAD)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}
	if docProgress.Metadata.BlockCount != docDirect.Metadata.BlockCount {
		t.Errorf("block count: progress=%d direct=%d", docProgress.Metadata.BlockCount, docDirect.Metadata.BlockCount)
	}
	if docProgress.Metadata.SubflowCount != docDirect.Metadata.SubflowCount {
		t.Errorf("subflow count: progress=%d direct=%d", docProgress.Metadata.SubflowCount, docDirect.Metadata.SubflowCount)
	}
}

func TestParseTextWithProgress_LargeFile_WithSubflows(t *testing.T) {
	text := `
#Region "Main"
CALL Helper
#End Region
#Region "Helper"
SET X TO 'done'
#End Region
`
	doc, err := ParseTextWithProgress(text, "multi.txt", 2_000_000, func(int, string) {})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if doc.Metadata.SubflowCount < 2 {
		t.Errorf("expected ≥2 subflows, got %d", doc.Metadata.SubflowCount)
	}
}

// TestParseTextWithProgress_UnclosedBlock_MatchesParseText locks in the fix
// for the drift that motivated unifying the pipeline: the legacy progress copy
// never flushed closable blocks left open at EOF, so it silently dropped the
// "unclosed block" ParseError that the canonical path reports. Both paths now
// run one Parser.Parse, so an abruptly-terminated LOOP must surface the same
// error count regardless of progress reporting.
func TestParseTextWithProgress_UnclosedBlock_MatchesParseText(t *testing.T) {
	// A LOOP opened with no matching END, wrapped in an implicit subflow.
	text := "LOOP WHILE %true%\n  SET X TO '1'\n"

	docDirect, err := ParseText(text, "test.txt", int64(len(text)))
	if err != nil {
		t.Fatalf("ParseText: %v", err)
	}

	docProgress, err := ParseTextWithProgress(text, "test.txt", 2_000_000, func(int, string) {})
	if err != nil {
		t.Fatalf("ParseTextWithProgress: %v", err)
	}

	if len(docProgress.ParseErrors) != len(docDirect.ParseErrors) {
		t.Fatalf("ParseErrors: progress=%d direct=%d (progress path dropped the EOF unclosed-block flush)",
			len(docProgress.ParseErrors), len(docDirect.ParseErrors))
	}
	if len(docDirect.ParseErrors) == 0 {
		t.Fatal("expected at least one unclosed-block ParseError on the canonical path")
	}
	for i := range docDirect.ParseErrors {
		if docProgress.ParseErrors[i].Message != docDirect.ParseErrors[i].Message {
			t.Errorf("ParseError[%d].Message: progress=%q direct=%q",
				i, docProgress.ParseErrors[i].Message, docDirect.ParseErrors[i].Message)
		}
	}
}
