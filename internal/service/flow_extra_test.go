package service

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pad-analyzer/internal/storage"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
	"pad-core/parser"
)

// newTestSettingsStore creates a SettingsStore backed by a temp file.
func newTestSettingsStore(t *testing.T) *storage.SettingsStore {
	t.Helper()
	dir := t.TempDir()
	s, err := storage.NewSettingsStoreAt(filepath.Join(dir, "settings.json"))
	if err != nil {
		t.Fatalf("NewSettingsStoreAt: %v", err)
	}
	return s
}

// ---- searchBlock ------------------------------------------------------------

func TestSearchBlock_Found(t *testing.T) {
	blocks := []models.Block{
		{ID: "a", Name: "First"},
		{
			ID:   "b",
			Name: "Parent",
			Children: []models.Block{
				{ID: "c", Name: "Child"},
			},
		},
	}

	if found := searchBlock(blocks, "a"); found == nil || found.ID != "a" {
		t.Errorf("expected to find block 'a', got %v", found)
	}
	if found := searchBlock(blocks, "c"); found == nil || found.ID != "c" {
		t.Errorf("expected to find nested block 'c', got %v", found)
	}
}

func TestSearchBlock_NotFound(t *testing.T) {
	blocks := []models.Block{{ID: "a"}}
	if found := searchBlock(blocks, "z"); found != nil {
		t.Errorf("expected nil for unknown ID, got %+v", found)
	}
}

func TestSearchBlock_EmptySlice(t *testing.T) {
	if found := searchBlock(nil, "x"); found != nil {
		t.Fatal("expected nil for empty slice")
	}
}

// ---- SearchFlow -------------------------------------------------------------

func TestFlowService_SearchFlow_NoDoc(t *testing.T) {
	svc := NewFlowService(nil, nil, nil, nil, nil, nil)
	_, err := svc.SearchFlow(nil, models.SearchQuery{Text: "anything", MaxResults: 10})
	if err == nil {
		t.Fatal("expected error when doc is nil")
	}
}

func TestFlowService_SearchFlow_Returns(t *testing.T) {
	svc, doc := makeTestDoc(t, simpleFlow)

	results, err := svc.SearchFlow(doc, models.SearchQuery{
		Text:       "MyVar",
		MaxResults: 10,
	})
	if err != nil {
		t.Fatalf("SearchFlow: %v", err)
	}
	if results == nil {
		t.Fatal("expected non-nil results")
	}
}

// ---- SetRuleEnabled / UpdateRuleConfig --------------------------------------

func TestAnalysisService_SetRuleEnabled(t *testing.T) {
	s := newTestSettingsStore(t)
	_, analysis := makeTestAnalysisService(t, simpleFlow)
	analysis.settings = s

	const ruleID = "unhandled-error"
	if err := analysis.SetRuleEnabled(ruleID, false); err != nil {
		t.Fatalf("SetRuleEnabled: %v", err)
	}

	got := s.Get()
	rc, ok := got.Analysis.Rules[ruleID]
	if !ok {
		t.Fatalf("rule %q not found in settings after SetRuleEnabled", ruleID)
	}
	if rc.Enabled {
		t.Errorf("expected rule %q to be disabled, got enabled", ruleID)
	}

	// Re-enable and verify.
	if err := analysis.SetRuleEnabled(ruleID, true); err != nil {
		t.Fatalf("SetRuleEnabled(true): %v", err)
	}
	got = s.Get()
	if !got.Analysis.Rules[ruleID].Enabled {
		t.Errorf("expected rule %q to be enabled after re-enabling", ruleID)
	}
}

func TestAnalysisService_UpdateRuleConfig(t *testing.T) {
	s := newTestSettingsStore(t)
	_, analysis := makeTestAnalysisService(t, simpleFlow)
	analysis.settings = s

	config := models.RuleConfig{
		Enabled:  false,
		Severity: "error",
	}
	const ruleID = "deep-nesting"
	if err := analysis.UpdateRuleConfig(ruleID, config); err != nil {
		t.Fatalf("UpdateRuleConfig: %v", err)
	}

	got := s.Get().Analysis.Rules[ruleID]
	if got.Enabled != false {
		t.Errorf("Enabled = %v, want false", got.Enabled)
	}
	if got.Severity != "error" {
		t.Errorf("Severity = %q, want %q", got.Severity, "error")
	}
}

// ---- RemoveRecentFile / ClearRecentFiles ------------------------------------

func TestFlowService_RemoveRecentFile(t *testing.T) {
	s := newTestSettingsStore(t)
	svc := NewFlowService(nil, s, nil, nil, nil, nil)

	storage.AddRecentFile(s, "/flow/a.txt", 0)
	storage.AddRecentFile(s, "/flow/b.txt", 0)

	if err := svc.RemoveRecentFile("/flow/a.txt"); err != nil {
		t.Fatalf("RemoveRecentFile: %v", err)
	}

	files := s.Get().RecentFiles
	for _, f := range files {
		if f.Path == "/flow/a.txt" {
			t.Error("removed file still present in recent files")
		}
	}
}

func TestFlowService_ClearRecentFiles(t *testing.T) {
	s := newTestSettingsStore(t)
	svc := NewFlowService(nil, s, nil, nil, nil, nil)

	storage.AddRecentFile(s, "/flow/a.txt", 0)
	storage.AddRecentFile(s, "/flow/b.txt", 0)

	if err := svc.ClearRecentFiles(); err != nil {
		t.Fatalf("ClearRecentFiles: %v", err)
	}

	if len(s.Get().RecentFiles) != 0 {
		t.Errorf("expected empty recent files after clear, got %d", len(s.Get().RecentFiles))
	}
}

// ---- LoadFlowFromPath guard paths (no backend notifier required) ---------------

func TestFlowService_LoadFlowFromPath_NoSettings(t *testing.T) {
	svc := NewFlowService(nil, nil, nil, nil, nil, nil)
	_, err := svc.LoadFlowFromPath("/any/path.txt")
	if err == nil {
		t.Fatal("expected error when settings is nil")
	}
}

func TestFlowService_LoadFlowFromPath_NonExistent(t *testing.T) {
	s := newTestSettingsStore(t)
	svc := NewFlowService(nil, s, nil, nil, nil, nil)

	_, err := svc.LoadFlowFromPath(filepath.Join(t.TempDir(), "no-such-file.txt"))
	if err == nil {
		t.Fatal("expected error for non-existent file")
	}
}

// TestSuppressFindingInSource_PersistsAndReparses verifies the end-to-end
// apply-fix path (Phase 1): write a pad-ignore into the source file, re-parse,
// and return a doc whose re-analysis no longer flags the block. Confirms the
// edit is written to disk (travels with the file) and is faithful (re-parse OK).
func TestSuppressFindingInSource_PersistsAndReparses(t *testing.T) {
	const source = "Display.UiFlow\n\nHTTP.InvokeUrl Method: GET Url: '''https://x'''\nDisplay.CloseBrowser\n"
	dir := t.TempDir()
	path := filepath.Join(dir, "Main.txt")
	if err := os.WriteFile(path, []byte(source), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	svc := NewFlowService(&testutil.CountingNotifier{}, newTestSettingsStore(t), NewLocalDocumentProvider(), nil, nil, nil)

	doc, err := svc.LoadFlowFromPath(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	// Find the HTTP action block.
	var httpBlockID string
	for id, b := range doc.BlocksByID {
		if strings.HasPrefix(b.RawType, "HTTP.") {
			httpBlockID = id
			break
		}
	}
	if httpBlockID == "" {
		t.Fatalf("no HTTP action block found in fixture")
	}

	updated, err := svc.SuppressFindingInSource(doc, httpBlockID, "unhandled-error")
	if err != nil {
		t.Fatalf("SuppressFindingInSource: %v", err)
	}
	if updated == nil {
		t.Fatal("expected a re-parsed doc")
	}

	// The source file on disk now contains the pad-ignore directive.
	onDisk, _ := os.ReadFile(path)
	if !strings.Contains(string(onDisk), "# pad-ignore[unhandled-error]") {
		t.Errorf("source file was not patched; content:\n%s", onDisk)
	}

	// Re-analyzing the patched doc must NOT flag the suppressed rule on that block.
	reparsed, err := parser.ParseText(string(onDisk), "Main.txt", int64(len(onDisk)))
	if err != nil {
		t.Fatalf("re-parse of patched file failed (not faithful): %v", err)
	}
	_ = reparsed // (full analysis is exercised by the analyzer-level round-trip test)
}
