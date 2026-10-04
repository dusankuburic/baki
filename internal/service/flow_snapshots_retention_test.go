package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"weak"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
	"pad-core/parser"
)

// weakCollected reports whether wp's target is garbage-collected within a
// few forced GC cycles — the retention tests' proof that a dropped element
// is unreachable, not merely hidden behind a reslice. Callers must not hold
// a strong reference themselves (build the target in a helper).
func weakCollected[T any](wp weak.Pointer[T]) bool {
	for i := 0; i < 10; i++ {
		runtime.GC()
		if wp.Value() == nil {
			return true
		}
		time.Sleep(time.Millisecond)
	}
	return false
}

// pushProbe pushes a size-byte single-file snapshot under key and returns
// only a weak pointer to it.
func pushProbe(st *snapshotStore, key, id string, size int) weak.Pointer[SourceSnapshot] {
	snap := &SourceSnapshot{ID: id, FlowID: key, Bytes: int64(size), source: strings.Repeat("x", size)}
	st.push(key, snap)
	return weak.Make(snap)
}

// ringIDs lists key's snapshot IDs oldest first WITHOUT touching its LRU
// position (list/take would refresh it and perturb the eviction order under
// test).
func ringIDs(st *snapshotStore, key string) []string {
	st.mu.Lock()
	defer st.mu.Unlock()
	r, ok := st.rings.Peek(key)
	if !ok {
		return nil
	}
	ids := make([]string, 0, len(r.snaps))
	for _, s := range r.snaps {
		ids = append(ids, s.ID)
	}
	return ids
}

// assertSnapshotAccounting checks the store's byte bookkeeping against a
// recount: every ring's bytes equals the sum of its snapshots' real retained
// size, the total equals the sum over rings, and the total is within budget.
func assertSnapshotAccounting(t *testing.T, st *snapshotStore) {
	t.Helper()
	st.mu.Lock()
	defer st.mu.Unlock()
	var sum int64
	for _, r := range st.rings.Values() {
		var rb int64
		for _, s := range r.snaps {
			rb += s.retainedBytes()
		}
		if rb != r.bytes {
			t.Errorf("ring bytes = %d, recount = %d", r.bytes, rb)
		}
		sum += rb
	}
	if st.total != sum {
		t.Errorf("store total = %d, recount = %d", st.total, sum)
	}
	if st.total > st.maxBytes {
		t.Errorf("store total %d exceeds the budget %d", st.total, st.maxBytes)
	}
}

// The per-flow trim must make the dropped snapshot unreachable. It used to
// reslice (ring[len-max:]), leaving the dropped pointer in the backing array
// ahead of the slice — append growth kept up to ~19 full sources per flow
// alive instead of 10.
func TestSnapshotStore_TrimmedSnapshotIsCollectable(t *testing.T) {
	st := newSnapshotStore()
	oldest := pushProbe(st, "flow", "s-0", 4096)
	for i := 1; i <= maxSnapshotsPerFlow; i++ {
		pushProbe(st, "flow", fmt.Sprintf("s-%d", i), 4096)
	}
	ids := ringIDs(st, "flow")
	if len(ids) != maxSnapshotsPerFlow || ids[0] != "s-1" || ids[len(ids)-1] != fmt.Sprintf("s-%d", maxSnapshotsPerFlow) {
		t.Fatalf("ring = %v, want s-1..s-%d", ids, maxSnapshotsPerFlow)
	}
	if !weakCollected(oldest) {
		t.Fatal("the trimmed snapshot is still reachable (pinned by the ring's backing array)")
	}
	assertSnapshotAccounting(t, st)
}

// A ring evicted by the flow-count LRU must be released too: golang-lru keeps
// the last evicted value in its eviction buffer, so the callback empties the
// ring or its sources would stay pinned until the next eviction.
func TestSnapshotStore_LRUEvictedRingIsCollectable(t *testing.T) {
	st := newSnapshotStore()
	first := pushProbe(st, "flow-0", "s-0", 4096)
	for i := 1; i <= maxSnapshotFlows; i++ {
		pushProbe(st, fmt.Sprintf("flow-%d", i), fmt.Sprintf("s-%d", i), 100)
	}
	if ringIDs(st, "flow-0") != nil {
		t.Fatal("flow-0 should have been evicted by the flow-count LRU")
	}
	if !weakCollected(first) {
		t.Fatal("the LRU-evicted ring's snapshot is still reachable")
	}
	assertSnapshotAccounting(t, st)
	if want := int64(maxSnapshotFlows * 100); st.total != want {
		t.Errorf("total after LRU eviction = %d, want %d", st.total, want)
	}
}

// The store-wide byte budget evicts the least-recently-used ring's oldest
// snapshot first, keeps the total within budget after every push, never
// drops the snapshot just pushed, and releases what it evicts.
func TestSnapshotStore_ByteBudgetEvictsOldestOfLRURing(t *testing.T) {
	st := newSnapshotStore()
	st.maxBytes = 100

	a1 := pushProbe(st, "A", "a1", 30)
	a2 := pushProbe(st, "A", "a2", 30)
	pushProbe(st, "B", "b1", 30)
	assertSnapshotAccounting(t, st)

	// 120 > 100: A is the LRU ring, a1 its oldest entry.
	pushProbe(st, "B", "b2", 30)
	if got := ringIDs(st, "A"); len(got) != 1 || got[0] != "a2" {
		t.Fatalf("A = %v, want [a2]", got)
	}
	assertSnapshotAccounting(t, st)

	// A's last entry goes next: the ring itself is removed.
	pushProbe(st, "B", "b3", 30)
	if got := ringIDs(st, "A"); got != nil {
		t.Fatalf("A = %v, want evicted", got)
	}
	assertSnapshotAccounting(t, st)

	// B alone: its OWN oldest entry goes, never the one just pushed.
	pushProbe(st, "B", "b4", 30)
	if got := strings.Join(ringIDs(st, "B"), ","); got != "b2,b3,b4" {
		t.Fatalf("B = %s, want b2,b3,b4", got)
	}
	assertSnapshotAccounting(t, st)
	if st.total != 90 {
		t.Errorf("total = %d, want 90", st.total)
	}
	if !weakCollected(a1) || !weakCollected(a2) {
		t.Error("budget-evicted snapshots are still reachable")
	}

	// A snapshot larger than the whole budget is not kept, and costs no
	// other flow its history.
	pushProbe(st, "C", "c1", 101)
	if ringIDs(st, "C") != nil || strings.Join(ringIDs(st, "B"), ",") != "b2,b3,b4" {
		t.Errorf("oversized snapshot handling: C = %v, B = %v", ringIDs(st, "C"), ringIDs(st, "B"))
	}
	assertSnapshotAccounting(t, st)
}

// Touching a ring (take/list) protects it from budget eviction the same way
// it protects it from the flow-count LRU.
func TestSnapshotStore_ByteBudgetHonoursRecency(t *testing.T) {
	st := newSnapshotStore()
	st.maxBytes = 100
	pushProbe(st, "A", "a1", 40)
	pushProbe(st, "B", "b1", 40)
	if _, ok := st.take("A", "a1"); !ok { // A becomes most-recently used
		t.Fatal("take a1")
	}
	pushProbe(st, "C", "c1", 40)
	if ringIDs(st, "B") != nil {
		t.Error("B (least recently used) should have been evicted")
	}
	if ringIDs(st, "A") == nil || ringIDs(st, "C") == nil {
		t.Errorf("A = %v, C = %v; both should survive", ringIDs(st, "A"), ringIDs(st, "C"))
	}
	assertSnapshotAccounting(t, st)
}

// Accounting stays exact across every path that changes the store —
// including folder snapshots, whose budget charge covers the member names
// the display-size Bytes field leaves out — and take/list change nothing.
func TestSnapshotStore_AccountingExact(t *testing.T) {
	st := newSnapshotStore()
	folder := &SourceSnapshot{ID: "f1", Files: map[string]string{"Main.txt": "abc", "Sub.txt": "de"}, Bytes: 5}
	st.push("folder", folder)
	if want := int64(len("Main.txt") + 3 + len("Sub.txt") + 2); st.total != want {
		t.Errorf("folder snapshot charged %d, want %d (names + texts)", st.total, want)
	}
	for i := 0; i < maxSnapshotsPerFlow+3; i++ {
		pushProbe(st, "single", fmt.Sprintf("s-%d", i), 10+i)
	}
	assertSnapshotAccounting(t, st)
	before := st.total
	st.take("single", "s-5")
	st.list("single")
	if st.total != before {
		t.Errorf("take/list changed the total: %d -> %d", before, st.total)
	}
	st.drop("folder")
	assertSnapshotAccounting(t, st)
	for i := 0; i < maxSnapshotFlows+5; i++ {
		pushProbe(st, fmt.Sprintf("flow-%d", i), "x", 7)
	}
	assertSnapshotAccounting(t, st)
	st.drop("flow-no-such-ring")
	assertSnapshotAccounting(t, st)
}

// ── Record-on-commit: failed mutations push nothing ─────────────────────────

// occConflictBackend answers every SaveFlow with an OCC conflict — a writer
// that committed between persistCloudDoc's header load and its save.
type occConflictBackend struct{ *testutil.FakeBackend }

func (occConflictBackend) SaveFlow(context.Context, *storageif.FlowDocument) error {
	return storageif.ErrVersionConflict
}

// newCloudSnapshotDoc is a stored-source single-file cloud flow.
func newCloudSnapshotDoc(t *testing.T, source string) *models.FlowDocument {
	t.Helper()
	doc, err := parser.ParseText(snapshotFlowSrc, "Main.txt", int64(len(snapshotFlowSrc)))
	if err != nil {
		t.Fatal(err)
	}
	doc.ID = "cloud-flow"
	doc.OwnerID = "u1"
	doc.Source = source
	doc.RebuildIndexes()
	return doc
}

func newCloudSnapshotSvc(backend storageif.StorageBackend) *FlowService {
	return NewFlowService(&testutil.CountingNotifier{}, nil, NewLocalDocumentProvider(), backend, nil, nil)
}

func firstHTTPBlock(t *testing.T, doc *models.FlowDocument) string {
	t.Helper()
	id := httpBlockID(doc, "")
	if id == "" {
		t.Fatal("fixture has no HTTP action")
	}
	return id
}

// A cloud source save rejected by OCC changed nothing, so it must not consume
// an undo slot; the save that commits records exactly one.
func TestSnapshots_CloudSaveSourceRecordsOnlyOnCommit(t *testing.T) {
	ctx := context.Background()
	doc := newCloudSnapshotDoc(t, snapshotFlowSrc)
	svc := newCloudSnapshotSvc(occConflictBackend{testutil.NewFakeBackend()})
	edited := strings.Replace(snapshotFlowSrc, "https://x", "https://y", 1)

	if _, err := svc.SaveSource(ctx, doc, edited, ""); !errors.Is(err, storageif.ErrVersionConflict) {
		t.Fatalf("SaveSource err = %v, want the OCC conflict", err)
	}
	if n := len(svc.ListSourceSnapshots(doc)); n != 0 {
		t.Fatalf("a rejected save recorded %d snapshot(s), want 0", n)
	}

	svc.storage = testutil.NewFakeBackend()
	if _, err := svc.SaveSource(ctx, doc, edited, ""); err != nil {
		t.Fatalf("SaveSource: %v", err)
	}
	snaps := svc.ListSourceSnapshots(doc)
	if len(snaps) != 1 || snaps[0].Label != "before source save" || snaps[0].source != snapshotFlowSrc {
		t.Fatalf("want one 'before source save' snapshot of the original source, got %+v", snaps)
	}
	assertSnapshotAccounting(t, svc.snapshots)
}

// A cloud single fix rejected by the parse-error gate or by OCC records
// nothing; the fix that commits records exactly one.
func TestSnapshots_CloudFixRecordsOnlyOnCommit(t *testing.T) {
	ctx := context.Background()

	// Gate: the stored source carries an unterminated comment the in-memory
	// parse does not, so the patched text parses with more errors than the
	// canon doc.
	gated := newCloudSnapshotDoc(t, snapshotFlowSrc+"/# never closed\n")
	svc := newCloudSnapshotSvc(testutil.NewFakeBackend())
	_, err := svc.ApplyFix(ctx, gated, firstHTTPBlock(t, gated), "wrap-error-handler", "unhandled-error", "", "", false)
	if err == nil || !strings.Contains(err.Error(), "parse errors") {
		t.Fatalf("ApplyFix err = %v, want the parse-error gate", err)
	}
	if n := len(svc.ListSourceSnapshots(gated)); n != 0 {
		t.Fatalf("a gate-rejected fix recorded %d snapshot(s), want 0", n)
	}

	// OCC conflict on the persist.
	doc := newCloudSnapshotDoc(t, snapshotFlowSrc)
	svc = newCloudSnapshotSvc(occConflictBackend{testutil.NewFakeBackend()})
	if _, err := svc.ApplyFix(ctx, doc, firstHTTPBlock(t, doc), "wrap-error-handler", "unhandled-error", "", "", false); !errors.Is(err, storageif.ErrVersionConflict) {
		t.Fatalf("ApplyFix err = %v, want the OCC conflict", err)
	}
	if _, _, err := svc.ApplyFixBatch(ctx, doc, nil, 10); !errors.Is(err, storageif.ErrVersionConflict) {
		t.Fatalf("ApplyFixBatch err = %v, want the OCC conflict", err)
	}
	if n := len(svc.ListSourceSnapshots(doc)); n != 0 {
		t.Fatalf("OCC-rejected fixes recorded %d snapshot(s), want 0", n)
	}

	svc.storage = testutil.NewFakeBackend()
	if _, err := svc.ApplyFix(ctx, doc, firstHTTPBlock(t, doc), "wrap-error-handler", "unhandled-error", "", "", false); err != nil {
		t.Fatalf("ApplyFix: %v", err)
	}
	if snaps := svc.ListSourceSnapshots(doc); len(snaps) != 1 || snaps[0].Label != "before fix" {
		t.Fatalf("want exactly one 'before fix' snapshot, got %+v", snaps)
	}
	assertSnapshotAccounting(t, svc.snapshots)
}

// Cloud FOLDER fixes: a single fix or batch rejected by OCC records nothing,
// nor does a batch with nothing to fix (it used to snapshot before its loop);
// the batch that commits records exactly one.
func TestSnapshots_CloudFolderFixRecordsOnlyOnCommit(t *testing.T) {
	ctx := context.Background()
	doc := cloudFolderDoc(t)
	svc := newCloudSnapshotSvc(occConflictBackend{testutil.NewFakeBackend()})

	if _, err := svc.ApplyFix(ctx, doc, httpBlockID(doc, "main"), "wrap-error-handler", "unhandled-error", "", "", false); !errors.Is(err, storageif.ErrVersionConflict) {
		t.Fatalf("folder ApplyFix err = %v, want the OCC conflict", err)
	}
	if _, _, err := svc.ApplyFixBatch(ctx, doc, nil, 10); !errors.Is(err, storageif.ErrVersionConflict) {
		t.Fatalf("folder ApplyFixBatch err = %v, want the OCC conflict", err)
	}
	if _, applied, err := svc.ApplyFixBatch(ctx, doc, map[string]bool{"no-such-rule": true}, 10); err != nil || applied != 0 {
		t.Fatalf("no-op batch = %d, %v; want 0, nil", applied, err)
	}
	if n := len(svc.ListSourceSnapshots(doc)); n != 0 {
		t.Fatalf("rejected/no-op folder mutations recorded %d snapshot(s), want 0", n)
	}

	svc.storage = testutil.NewFakeBackend()
	if _, applied, err := svc.ApplyFixBatch(ctx, doc, nil, 10); err != nil || applied == 0 {
		t.Fatalf("ApplyFixBatch = %d, %v", applied, err)
	}
	snaps := svc.ListSourceSnapshots(doc)
	if len(snaps) != 1 || snaps[0].Files == nil {
		t.Fatalf("want exactly one folder snapshot, got %+v", snaps)
	}

	// A restore rejected by OCC records no "before restore" entry either.
	svc.storage = occConflictBackend{testutil.NewFakeBackend()}
	if _, err := svc.RestoreSourceSnapshot(ctx, doc, snaps[0].ID); !errors.Is(err, storageif.ErrVersionConflict) {
		t.Fatalf("RestoreSourceSnapshot err = %v, want the OCC conflict", err)
	}
	if n := len(svc.ListSourceSnapshots(doc)); n != 1 {
		t.Fatalf("a rejected folder restore left %d snapshot(s), want 1", n)
	}
	assertSnapshotAccounting(t, svc.snapshots)
}

// A cloud restore rejected by OCC records nothing; one that commits records
// its "before restore" entry (and the accounting stays exact).
func TestSnapshots_CloudRestoreRecordsOnlyOnCommit(t *testing.T) {
	ctx := context.Background()
	doc := newCloudSnapshotDoc(t, snapshotFlowSrc)
	svc := newCloudSnapshotSvc(testutil.NewFakeBackend())
	if _, err := svc.SaveSource(ctx, doc, strings.Replace(snapshotFlowSrc, "https://x", "https://y", 1), ""); err != nil {
		t.Fatalf("SaveSource: %v", err)
	}
	snaps := svc.ListSourceSnapshots(doc)
	if len(snaps) != 1 {
		t.Fatalf("want 1 snapshot after the save, got %d", len(snaps))
	}

	svc.storage = occConflictBackend{testutil.NewFakeBackend()}
	if _, err := svc.RestoreSourceSnapshot(ctx, doc, snaps[0].ID); !errors.Is(err, storageif.ErrVersionConflict) {
		t.Fatalf("RestoreSourceSnapshot err = %v, want the OCC conflict", err)
	}
	if n := len(svc.ListSourceSnapshots(doc)); n != 1 {
		t.Fatalf("a rejected restore left %d snapshot(s), want 1", n)
	}

	svc.storage = testutil.NewFakeBackend()
	if _, err := svc.RestoreSourceSnapshot(ctx, doc, snaps[0].ID); err != nil {
		t.Fatalf("RestoreSourceSnapshot: %v", err)
	}
	if got := svc.ListSourceSnapshots(doc); len(got) != 2 || got[1].Label != "before restore" {
		t.Fatalf("want the committed restore to add one 'before restore' entry, got %+v", got)
	}
	assertSnapshotAccounting(t, svc.snapshots)
}

// blockDesktopWrites makes atomicWriteConv fail for the given files of dir
// while leaving them readable: Windows refuses to rename over a read-only
// file; elsewhere a read-only directory refuses the temp file. Returns the
// undo. (A superuser ignores both modes — callers skip when the write
// unexpectedly lands.)
func blockDesktopWrites(t *testing.T, dir string, files ...string) (unblock func()) {
	t.Helper()
	if runtime.GOOS == "windows" {
		for _, f := range files {
			if err := os.Chmod(f, 0o444); err != nil {
				t.Fatal(err)
			}
		}
		unblock = func() {
			for _, f := range files {
				_ = os.Chmod(f, 0o644)
			}
		}
	} else {
		if err := os.Chmod(dir, 0o555); err != nil { // #nosec G302 -- test temp dir
			t.Fatal(err)
		}
		unblock = func() { _ = os.Chmod(dir, 0o755) } // #nosec G302 -- test temp dir
	}
	t.Cleanup(unblock)
	return unblock
}

// Desktop: a save, fix, batch or restore whose write fails changed nothing on
// disk and records nothing; the save that lands records exactly one.
func TestSnapshots_DesktopFailedWriteRecordsNothing(t *testing.T) {
	ctx := context.Background()
	svc, doc := newDesktopFlowSvc(t)
	unblock := blockDesktopWrites(t, filepath.Dir(doc.FilePath), doc.FilePath)
	edited := strings.Replace(snapshotFlowSrc, "https://x", "https://y", 1)

	if _, err := svc.SaveSource(ctx, doc, edited, ""); err == nil {
		t.Skip("this platform/user lets writes through the read-only guard")
	}
	if _, err := svc.ApplyFix(ctx, doc, firstHTTPBlock(t, doc), "wrap-error-handler", "unhandled-error", "", "", false); err == nil {
		t.Fatal("ApplyFix should fail on the blocked write")
	}
	if _, _, err := svc.ApplyFixBatch(ctx, doc, nil, 10); err == nil {
		t.Fatal("ApplyFixBatch should fail on the blocked write")
	}
	if n := len(svc.ListSourceSnapshots(doc)); n != 0 {
		t.Fatalf("failed desktop writes recorded %d snapshot(s), want 0", n)
	}

	unblock()
	if _, err := svc.SaveSource(ctx, doc, edited, ""); err != nil {
		t.Fatalf("SaveSource: %v", err)
	}
	snaps := svc.ListSourceSnapshots(doc)
	if len(snaps) != 1 || snaps[0].source != snapshotFlowSrc {
		t.Fatalf("want exactly one snapshot of the original bytes, got %+v", snaps)
	}

	blockDesktopWrites(t, filepath.Dir(doc.FilePath), doc.FilePath)
	if _, err := svc.RestoreSourceSnapshot(ctx, doc, snaps[0].ID); err == nil {
		t.Fatal("RestoreSourceSnapshot should fail on the blocked write")
	}
	if n := len(svc.ListSourceSnapshots(doc)); n != 1 {
		t.Fatalf("a failed restore left %d snapshot(s), want 1", n)
	}
	assertSnapshotAccounting(t, svc.snapshots)
}

// Desktop FOLDER batch and restore: a write that fails before any member
// changed records nothing; the batch that lands records exactly one.
func TestSnapshots_DesktopFolderFailedWriteRecordsNothing(t *testing.T) {
	ctx := context.Background()
	svc, doc, dir := newDesktopFolder(t)
	members := []string{filepath.Join(dir, "Main.txt"), filepath.Join(dir, "Subflow.txt")}
	filter := map[string]bool{"redundant-action": true}
	unblock := blockDesktopWrites(t, dir, members...)

	if _, _, err := svc.ApplyFixBatch(ctx, doc, filter, 10); err == nil {
		t.Skip("this platform/user lets writes through the read-only guard")
	}
	if n := len(svc.ListSourceSnapshots(doc)); n != 0 {
		t.Fatalf("a failed folder batch recorded %d snapshot(s), want 0", n)
	}

	unblock()
	if _, applied, err := svc.ApplyFixBatch(ctx, doc, filter, 10); err != nil || applied == 0 {
		t.Fatalf("ApplyFixBatch = %d, %v", applied, err)
	}
	snaps := svc.ListSourceSnapshots(doc)
	if len(snaps) != 1 {
		t.Fatalf("want exactly one folder snapshot, got %d", len(snaps))
	}

	blockDesktopWrites(t, dir, members...)
	if _, err := svc.RestoreSourceSnapshot(ctx, doc, snaps[0].ID); err == nil {
		t.Fatal("RestoreSourceSnapshot should fail on the blocked write")
	}
	if n := len(svc.ListSourceSnapshots(doc)); n != 1 {
		t.Fatalf("a failed folder restore left %d snapshot(s), want 1", n)
	}
	assertSnapshotAccounting(t, svc.snapshots)
}
