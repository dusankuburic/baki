package service

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pad-analyzer/internal/ai"
	"pad-core/models"
)

// TestSourceFilesFingerprint verifies the cache-key fragment is order-stable
// (so a re-ordering of the same selection still hits) and empty when nothing is
// selected.
func TestSourceFilesFingerprint(t *testing.T) {
	if got := sourceFilesFingerprint(nil); got != "" {
		t.Errorf("nil files fingerprint = %q, want empty", got)
	}
	a := sourceFilesFingerprint([]string{"b.txt", "a.txt"})
	b := sourceFilesFingerprint([]string{"a.txt", "b.txt"})
	if a != b {
		t.Errorf("fingerprint not order-stable: %q vs %q", a, b)
	}
	if !strings.Contains(a, "a.txt") || !strings.Contains(a, "b.txt") {
		t.Errorf("fingerprint missing names: %q", a)
	}
}

// ctxCacheStubProvider2 duplicates the minimal stub used in chat_test.go (same
// package), kept local so this test file is self-contained for the wiring check.
type ctxCacheStubProvider2 struct{ ai.Provider }

func (ctxCacheStubProvider2) ID() string                  { return "stub" }
func (ctxCacheStubProvider2) EstimateTokens(s string) int { return len(s) / 4 }
func (ctxCacheStubProvider2) ContextLimit() int           { return 100000 }

// TestComputeContextCore_SourceFilesWired verifies C-10: a request carrying
// SelectedSourceFiles now has those files read and injected into the built
// context (previously the field was collected then silently dropped).
// ExcludeContext must skip the sources along with the rest of context.
func TestComputeContextCore_SourceFilesWired(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("UNIQUE_SOURCE_MARKER_42"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	doc := &models.FlowDocument{ID: "f1", FilePath: filepath.Join(dir, "flow.txt"), Name: "f1"}
	doc.RebuildIndexes()

	svc := &ChatService{
		chatCtxCache: newChatContextCache(),
		flowCache:    &FlowService{},
	}
	provider := ctxCacheStubProvider2{}

	// With the source selected, its content reaches the built context text.
	req := models.ChatRequest{
		Model:               "m1",
		UserMessage:         "q",
		SelectedSourceFiles: []string{"extra.txt"},
	}
	cv := svc.cachedContextCore(context.Background(), "scope", provider, doc, nil, req)
	if !strings.Contains(cv.contextText, "UNIQUE_SOURCE_MARKER_42") {
		t.Errorf("expected source content in contextText, got: %q", cv.contextText)
	}

	// A second identical call is a cache hit (same selection) — no re-read, same pointer.
	cv2 := svc.cachedContextCore(context.Background(), "scope", provider, doc, nil, req)
	if cv.contextText != cv2.contextText {
		t.Error("expected cache hit for the same selection")
	}

	// ExcludeContext skips the source files (free-form question path).
	reqExc := req
	reqExc.ExcludeContext = true
	cvExc := svc.computeContextCore(provider, doc, nil, reqExc)
	if strings.Contains(cvExc.contextText, "UNIQUE_SOURCE_MARKER_42") {
		t.Error("ExcludeContext should skip source-file injection")
	}

	// A different selection misses the cache (different fingerprint in the key).
	reqOther := models.ChatRequest{Model: "m1", UserMessage: "q", SelectedSourceFiles: []string{"other.txt"}}
	cvOther := svc.cachedContextCore(context.Background(), "scope", provider, doc, nil, reqOther)
	if strings.Contains(cvOther.contextText, "UNIQUE_SOURCE_MARKER_42") {
		t.Error("a different selection must not reuse another selection's cached sources")
	}
}

// TestComputeContextCore_ExcludeContextGating covers BUG-2 and BUG-4:
//   - BUG-2: ExcludeContext is part of the cache key, so a free-form turn and a
//     context-bearing turn (same selection) get distinct cache entries — the
//     context-bearing turn must NOT receive the source-less contextText.
//   - BUG-4: ExcludeContext gates the SelectedBlock context injection (not just
//     source files), so a free-form turn on a block-scoped thread omits the
//     block detail from contextText.
func TestComputeContextCore_ExcludeContextGating(t *testing.T) {
	dir := t.TempDir()
	doc := &models.FlowDocument{
		ID: "f1", FilePath: filepath.Join(dir, "flow.txt"), Name: "f1",
		Subflows: []models.Subflow{{ID: "sf1", Name: "Main",
			Blocks: []models.Block{{ID: "blk1", Name: "Action", Type: models.BlockTypeAction, RawType: "Foo.Bar"}}}},
	}
	doc.RebuildIndexes()
	if err := os.WriteFile(filepath.Join(dir, "extra.txt"), []byte("SOURCE_MARKER"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}

	svc := &ChatService{chatCtxCache: newChatContextCache(), flowCache: &FlowService{}}
	provider := ctxCacheStubProvider2{}

	base := models.ChatRequest{Model: "m1", UserMessage: "q", ContextBlockID: "blk1", SelectedSourceFiles: []string{"extra.txt"}}

	// Free-form turn first (ExcludeContext=true): no block detail, no sources.
	exc := base
	exc.ExcludeContext = true
	cvExc := svc.cachedContextCore(context.Background(), "scope", provider, doc, nil, exc)
	if strings.Contains(cvExc.contextText, "SOURCE_MARKER") {
		t.Error("ExcludeContext=true must not inject source files")
	}
	if strings.Contains(cvExc.contextText, "Selected Block") {
		t.Error("ExcludeContext=true must not inject the selected block detail (BUG-4)")
	}

	// Context-bearing turn (ExcludeContext=false), same key inputs otherwise:
	// MUST be a cache miss (BUG-2) → sources + block detail present.
	inc := base
	inc.ExcludeContext = false
	cvInc := svc.cachedContextCore(context.Background(), "scope", provider, doc, nil, inc)
	if !strings.Contains(cvInc.contextText, "SOURCE_MARKER") {
		t.Error("ExcludeContext=false must inject source files (BUG-2: cache must not serve the ExcludeContext=true entry)")
	}
	if !strings.Contains(cvInc.contextText, "Selected Block") {
		t.Error("ExcludeContext=false must inject the selected block detail")
	}
}
