package service

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"pad-analyzer/internal/testutil"
)

// TestDesktopFlowIdentity_SurvivesReparse pins R-analyzer-boundary-c6: the parser mints a fresh document id on
// every parse, and the desktop chat keys its threads and stored conversation on that id. A restart (a new
// FlowService, nothing cached), an applied fix, a content-changing save and a snapshot restore each handed
// the chat a "new" flow, orphaning the thread and its history. A path-backed flow now keeps one id.
func TestDesktopFlowIdentity_SurvivesReparse(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "Main.txt")
	if err := os.WriteFile(path, []byte(snapshotFlowSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	newSvc := func() *FlowService {
		return NewFlowService(&testutil.CountingNotifier{}, newTestSettingsStore(t), NewLocalDocumentProvider(), nil, nil, nil)
	}

	first, err := newSvc().LoadFlowFromPath(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	svc := newSvc() // a restart: another process, nothing cached
	doc, err := svc.LoadFlowFromPath(path)
	if err != nil {
		t.Fatalf("reload: %v", err)
	}
	if doc.ID != first.ID {
		t.Fatalf("the same file got another id after a restart: %q then %q", first.ID, doc.ID)
	}
	id := doc.ID

	fixed, err := svc.ApplyFix(ctx, doc, httpBlockID(doc, "https://x"), "wrap-error-handler", "unhandled-error", "", "", false)
	if err != nil {
		t.Fatalf("ApplyFix: %v", err)
	}
	if fixed.ID != id {
		t.Errorf("an applied fix changed the flow id: %q -> %q", id, fixed.ID)
	}

	saved, err := svc.SaveSource(ctx, fixed, snapshotFlowSrc+"\n# edited\n", "")
	if err != nil {
		t.Fatalf("SaveSource: %v", err)
	}
	if saved.ID != id {
		t.Errorf("a content-changing save changed the flow id: %q -> %q", id, saved.ID)
	}

	snaps := snapshotsOf(t, svc, saved)
	if len(snaps) == 0 {
		t.Fatal("no snapshot to restore")
	}
	restored, err := svc.RestoreSourceSnapshot(ctx, saved, snaps[0].ID)
	if err != nil {
		t.Fatalf("RestoreSourceSnapshot: %v", err)
	}
	if restored.ID != id {
		t.Errorf("a snapshot restore changed the flow id: %q -> %q", id, restored.ID)
	}
}

// TestDesktopFlowIdentity_DistinctPathsDistinctIDs: the same bytes under two paths are two flows (two
// threads, two conversations), even when they share one AST-cache entry.
func TestDesktopFlowIdentity_DistinctPathsDistinctIDs(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a", "Main.txt"), filepath.Join(dir, "b", "Main.txt")
	for _, p := range []string{a, b} {
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(snapshotFlowSrc), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	svc := NewFlowService(&testutil.CountingNotifier{}, newTestSettingsStore(t), NewLocalDocumentProvider(), nil, nil, NewASTCache(1<<30))
	da, err := svc.LoadFlowFromPath(a)
	if err != nil {
		t.Fatal(err)
	}
	db, err := svc.LoadFlowFromPath(b)
	if err != nil {
		t.Fatal(err)
	}
	if da.ID == db.ID {
		t.Errorf("two paths with the same bytes share an id %q", da.ID)
	}
	if da.FilePath != a || db.FilePath != b {
		t.Errorf("file paths crossed over the shared cache entry: %q, %q", da.FilePath, db.FilePath)
	}
}
