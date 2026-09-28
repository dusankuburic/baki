package api

import (
	"encoding/json"
	"testing"
)

// Stored content is client-supplied: the diff path must not carry a desktop
// file path out of it.
func TestUnmarshalFlowContent_DropsClientSuppliedFilePath(t *testing.T) {
	content, err := json.Marshal(map[string]any{"id": "x", "name": "n", "filePath": "/etc/passwd"})
	if err != nil {
		t.Fatal(err)
	}
	doc, err := unmarshalFlowContent(content)
	if err != nil {
		t.Fatal(err)
	}
	if doc.FilePath != "" {
		t.Fatalf("FilePath = %q, want it cleared", doc.FilePath)
	}
}
