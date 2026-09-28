package scanner

import (
	"encoding/json"
	"testing"
)

// Stored content is client-supplied: the sweep must not inherit a desktop
// file path from it (it keys the analysis cache and history on the path).
func TestParseStored_DropsClientSuppliedFilePath(t *testing.T) {
	content, err := json.Marshal(map[string]any{"id": "x", "name": "n", "filePath": "/etc/passwd"})
	if err != nil {
		t.Fatal(err)
	}
	doc := parseStored(content)
	if doc == nil {
		t.Fatal("parseStored returned nil")
	}
	if doc.FilePath != "" {
		t.Fatalf("FilePath = %q, want it cleared", doc.FilePath)
	}
}
