package service

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pad-analyzer/internal/testutil"
	"pad-core/models"
	"pad-core/parser"
)

const snapshotFlowSrc = "#Region \"Main\"\n    HTTPClient.InvokeUrl Url: $'''https://x''' Method: HTTPClient.Method.GET\n#EndRegion\n"

func newDesktopFlowSvc(t *testing.T) (*FlowService, *models.FlowDocument) {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "Main.txt")
	if err := os.WriteFile(path, []byte(snapshotFlowSrc), 0o644); err != nil {
		t.Fatal(err)
	}
	doc, err := parser.ParseText(snapshotFlowSrc, "Main.txt", int64(len(snapshotFlowSrc)))
	if err != nil {
		t.Fatal(err)
	}
	doc.FilePath = path
	doc.RebuildIndexes()
	ldp := NewLocalDocumentProvider()
	ldp.SetCurrentDoc(doc)
	// loadAndParse (the restore reload path) requires a settings store.
	svc := NewFlowService(&testutil.CountingNotifier{}, newTestSettingsStore(t), ldp, nil, nil, nil)
	return svc, doc
}

// TestSnapshots_FixCaptureAndRestore pins R1-2: a desktop auto-fix captures
// the pre-fix source in the undo ring (labelled), and restoring writes the
// original bytes back and returns the re-loaded document.
func TestSnapshots_FixCaptureAndRestore(t *testing.T) {
	svc, doc := newDesktopFlowSvc(t)
	ctx := context.Background()

	var blockID string
	for _, sf := range doc.Subflows {
		for i := range sf.Blocks {
			if sf.Blocks[i].RawType == "HTTPClient.InvokeUrl" {
				blockID = sf.Blocks[i].ID
			}
		}
	}
	if blockID == "" {
		t.Fatal("fixture has no HTTP action")
	}

	fixed, err := svc.ApplyFix(ctx, doc, blockID, "wrap-error-handler", "unhandled-error", "", "")
	if err != nil {
		t.Fatalf("ApplyFix: %v", err)
	}
	if !strings.Contains(fixed.Source, "ON BLOCK ERROR") && !containsOnDisk(t, doc.FilePath, "ON BLOCK ERROR") {
		t.Fatal("fix did not land")
	}

	snaps := svc.ListSourceSnapshots(doc)
	if len(snaps) != 1 {
		t.Fatalf("want 1 snapshot after the fix, got %d", len(snaps))
	}
	if snaps[0].Label != "before fix" {
		t.Errorf("label = %q, want 'before fix'", snaps[0].Label)
	}
	if snaps[0].Bytes != int64(len(snapshotFlowSrc)) {
		t.Errorf("bytes = %d, want %d", snaps[0].Bytes, len(snapshotFlowSrc))
	}

	restored, err := svc.RestoreSourceSnapshot(ctx, doc, snaps[0].ID)
	if err != nil {
		t.Fatalf("RestoreSourceSnapshot: %v", err)
	}
	// Desktop authority is the FILE: the rolled-back bytes are on disk, and
	// the re-loaded document has no trace of the handler wrap.
	if !containsOnDisk(t, doc.FilePath, "HTTPClient.InvokeUrl") || containsOnDisk(t, doc.FilePath, "ON BLOCK ERROR") {
		t.Errorf("restore did not roll the fix back on disk")
	}
	if containsOnDisk(t, doc.FilePath, "ON BLOCK ERROR") || strings.Contains(restored.Source, "ON BLOCK ERROR") {
		t.Errorf("handler wrap survived the restore")
	}
	// The restore itself snapshotted the FIXED state → undoable too.
	if got := len(svc.ListSourceSnapshots(doc)); got != 2 {
		t.Errorf("restore should add its own snapshot, got %d total", got)
	}
}

// TestSnapshots_RingBounded: the ring caps at maxSnapshotsPerFlow, keeping
// the NEWEST entries.
func TestSnapshots_RingBounded(t *testing.T) {
	svc, doc := newDesktopFlowSvc(t)
	for i := 0; i < maxSnapshotsPerFlow+5; i++ {
		svc.snapshotDesktopFile(doc, doc.FilePath, "iter")
	}
	snaps := svc.ListSourceSnapshots(doc)
	if len(snaps) != maxSnapshotsPerFlow {
		t.Fatalf("ring size = %d, want %d", len(snaps), maxSnapshotsPerFlow)
	}
}

func containsOnDisk(t *testing.T, path, needle string) bool {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	return strings.Contains(string(data), needle)
}

// B1.1: the rings map is LRU-bounded — a long-lived instance editing many
// flows used to retain up to maxSnapshotsPerFlow full source copies per flow
// forever.
func TestSnapshotStore_LRUBounded(t *testing.T) {
	st := newSnapshotStore()
	for i := 0; i < maxSnapshotFlows+10; i++ {
		st.push(fmt.Sprintf("flow-%d", i), &SourceSnapshot{ID: fmt.Sprintf("s-%d", i)})
	}
	if got := st.rings.Len(); got != maxSnapshotFlows {
		t.Errorf("rings.Len() = %d, want %d", got, maxSnapshotFlows)
	}
	// Oldest flows evicted; newest still present.
	if _, ok := st.take("flow-0", "s-0"); ok {
		t.Error("oldest flow ring should have been evicted")
	}
	if _, ok := st.take(fmt.Sprintf("flow-%d", maxSnapshotFlows+9), fmt.Sprintf("s-%d", maxSnapshotFlows+9)); !ok {
		t.Error("newest flow ring missing")
	}
	// Touching refreshes LRU position: flow-A used, then many others pushed —
	// A survives because it was touched LAST.
	st.take("flow-A-ref", "x") // miss is fine
	st.push("keep", &SourceSnapshot{ID: "k1"})
	for i := 0; i < maxSnapshotFlows-1; i++ {
		st.push(fmt.Sprintf("other-%d", i), &SourceSnapshot{ID: "x"})
	}
	if _, ok := st.take("keep", "k1"); !ok {
		t.Error("recently-touched ring was evicted too early")
	}
	// drop() removes explicitly.
	st.drop("keep")
	if _, ok := st.take("keep", "k1"); ok {
		t.Error("drop did not remove the ring")
	}
}
