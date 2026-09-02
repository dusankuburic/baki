package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pad-core/models"
)

// TestComputeContextCore_ScrubsSelectedSourceFiles pins S1: selected source
// files are AST-scrubbed before injection into the model context. PAD's
// `SET Password TO $”'secret”'` syntax matches no key=value regex, so raw
// disk bytes used to reach the model verbatim — bypassing the property-name
// masking the document path gets.
func TestComputeContextCore_ScrubsSelectedSourceFiles(t *testing.T) {
	dir := t.TempDir()
	const secret = "super-secret-password"
	src := "FUNCTION Main\n    SET Password TO $'''" + secret + "'''\nEND"
	if err := os.WriteFile(filepath.Join(dir, "sub.txt"), []byte(src), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	svc := &ChatService{flowCache: &FlowService{}}
	doc := &models.FlowDocument{
		ID:       "f1",
		FilePath: filepath.Join(dir, "flow.txt"),
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main", Blocks: []models.Block{
			{ID: "b1", Name: "Xavier", Type: models.BlockTypeAction, RawType: "Foo.Bar"},
		}}},
	}
	doc.RebuildIndexes()

	cv := svc.computeContextCore(ctxCacheStubProvider{}, doc, nil, models.ChatRequest{
		SelectedSourceFiles: []string{"sub.txt"},
	})

	if strings.Contains(cv.contextText, secret) {
		t.Errorf("source-file secret leaked into model context:\n%s", cv.contextText)
	}
	if !strings.Contains(cv.contextText, "sub.txt") {
		t.Errorf("source file context missing entirely (read path broken?):\n%s", cv.contextText)
	}
}

// TestComputeContextCore_SourceFilesExcludedWithoutFlowPath: the FilePath
// guard must keep the source injection off for cloud docs (empty FilePath) —
// and the S1 scrub must not change that.
func TestComputeContextCore_SourceFilesExcludedWithoutFlowPath(t *testing.T) {
	svc := &ChatService{flowCache: &FlowService{}}
	doc := &models.FlowDocument{ID: "f1"}

	cv := svc.computeContextCore(ctxCacheStubProvider{}, doc, nil, models.ChatRequest{
		SelectedSourceFiles: []string{"sub.txt"},
	})

	if strings.Contains(cv.contextText, "sub.txt") {
		t.Errorf("source file read without a flow directory — path traversal guard regressed")
	}
	_ = context.Background()
}
