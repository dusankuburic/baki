package service

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// Stored flow content is client-supplied (POST/PUT /api/library store it
// verbatim) and FlowDocument.FilePath is a serialized field. A poisoned
// "filePath" used to survive rehydration and send cloud requests down the
// desktop file branch of GetSource/SaveSource: arbitrary server file read
// and write for any member.
func TestResolveStoredFlow_DropsClientSuppliedFilePath(t *testing.T) {
	secret := filepath.Join(t.TempDir(), "environ")
	if err := os.WriteFile(secret, []byte("PAD_AUTH_SECRET=top-secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	content, err := json.Marshal(map[string]any{"id": "parser-id", "name": "poisoned", "filePath": secret})
	if err != nil {
		t.Fatal(err)
	}

	doc, err := resolveStoredFlow("flow-1", &interfaces.FlowDocument{
		ID: "flow-1", OwnerID: "u1", Content: content, Source: "stored source",
	})
	if err != nil {
		t.Fatal(err)
	}
	if doc.FilePath != "" {
		t.Fatalf("FilePath = %q, want it cleared for a stored flow", doc.FilePath)
	}

	got, err := (&FlowService{}).GetSource(doc, "")
	if err != nil {
		t.Fatal(err)
	}
	if got != "stored source" {
		t.Fatalf("GetSource = %q, want the stored cloud source (never the file named in content)", got)
	}
}
