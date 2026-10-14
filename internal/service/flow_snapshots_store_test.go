package service

import (
	"context"
	"errors"
	"strings"
	"testing"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
	"pad-core/models"
)

// snapshotsOf lists doc's undo snapshots through svc, failing the test on an error.
func snapshotsOf(t testing.TB, svc *FlowService, doc *models.FlowDocument) []*SourceSnapshot {
	t.Helper()
	snaps, err := svc.ListSourceSnapshots(context.Background(), doc)
	if err != nil {
		t.Fatalf("ListSourceSnapshots: %v", err)
	}
	return snaps
}

// replica builds one more FlowService over the shared backend, as another process of the same deployment would: its
// own memory, the same database.
func replica(backend storageif.StorageBackend) *FlowService {
	return NewFlowService(&testutil.CountingNotifier{}, nil, NewLocalDocumentProvider(), backend, nil, nil, WithSnapshotStore(backend))
}

// A snapshot taken through one service instance is undoable through another on the same database: the list, the
// restore, and the "before restore" entry the restore records are all shared.
func TestSnapshotStore_TwoReplicasShareTheRing(t *testing.T) {
	ctx := context.Background()
	backend := testutil.NewFakeBackend()
	a, b := replica(backend), replica(backend)
	doc := newCloudSnapshotDoc(t, snapshotFlowSrc)
	edited := strings.Replace(snapshotFlowSrc, "https://x", "https://y", 1)

	updated, err := a.SaveSource(ctx, doc, edited, "")
	if err != nil {
		t.Fatalf("SaveSource on replica A: %v", err)
	}
	if got := len(a.snapshots.list(flowSnapshotKey(doc))); got != 0 {
		t.Fatalf("the durable ring also filled A's memory ring (%d entries)", got)
	}

	snaps := snapshotsOf(t, b, updated)
	if len(snaps) != 1 || snaps[0].Label != "before source save" || snaps[0].Bytes != int64(len(snapshotFlowSrc)) || snaps[0].FlowID != doc.ID || snaps[0].CreatedAt.IsZero() {
		t.Fatalf("replica B lists %+v, want the one 'before source save' snapshot A took", snaps)
	}

	restored, err := b.RestoreSourceSnapshot(ctx, updated, snaps[0].ID)
	if err != nil {
		t.Fatalf("RestoreSourceSnapshot on replica B: %v", err)
	}
	if restored.Source != snapshotFlowSrc {
		t.Errorf("restored source = %q, want the original", restored.Source)
	}
	stored, err := backend.LoadFlow(ctx, doc.ID)
	if err != nil || stored.Source != snapshotFlowSrc {
		t.Errorf("the stored flow after B's restore = %q, %v; want the original source", stored.Source, err)
	}

	// The restore recorded the state it replaced; both replicas list both entries, oldest first.
	for name, svc := range map[string]*FlowService{"A": a, "B": b} {
		got := snapshotsOf(t, svc, restored)
		if len(got) != 2 || got[0].ID != snaps[0].ID || got[1].Label != "before restore" {
			t.Errorf("replica %s lists %+v, want the save's snapshot then 'before restore'", name, got)
		}
	}
}

// A cloud fix and a folder batch fix survive onto the other replica too (a folder snapshot carries its member map).
func TestSnapshotStore_FixesAndFolderRestoreAcrossReplicas(t *testing.T) {
	ctx := context.Background()
	backend := testutil.NewFakeBackend()
	a, b := replica(backend), replica(backend)

	doc := newCloudSnapshotDoc(t, snapshotFlowSrc)
	fixed, err := a.ApplyFix(ctx, doc, firstHTTPBlock(t, doc), "wrap-error-handler", "unhandled-error", "", "", false)
	if err != nil {
		t.Fatalf("ApplyFix: %v", err)
	}
	if snaps := snapshotsOf(t, b, fixed); len(snaps) != 1 || snaps[0].Label != "before fix" {
		t.Fatalf("replica B lists %+v after A's fix", snaps)
	}

	folder := cloudFolderDoc(t)
	folder.ID, folder.OwnerID = "cloud-folder", "u1"
	res, applied, err := a.ApplyFixBatch(ctx, folder, nil, 10)
	if err != nil || applied == 0 {
		t.Fatalf("folder ApplyFixBatch = %d, %v", applied, err)
	}
	snaps := snapshotsOf(t, b, res)
	if len(snaps) != 1 || snaps[0].Label != "before batch fix" {
		t.Fatalf("replica B lists %+v after A's folder batch", snaps)
	}
	restored, err := b.RestoreSourceSnapshot(ctx, res, snaps[0].ID)
	if err != nil {
		t.Fatalf("folder restore on B: %v", err)
	}
	if !restored.IsFolder && len(restored.Subflows) == 0 {
		t.Errorf("the folder came back without its structure: %+v", restored.Name)
	}
	if got := snapshotsOf(t, a, restored); len(got) != 2 || got[1].Label != "before restore" {
		t.Errorf("replica A lists %+v after B's folder restore, want the batch's snapshot and 'before restore'", got)
	}
}

// An id from another flow restores nothing through this one, and an unknown id is the typed not-found the handler
// answers 404 to.
func TestSnapshotStore_RestoreIsScopedToTheFlow(t *testing.T) {
	ctx := context.Background()
	backend := testutil.NewFakeBackend()
	svc := replica(backend)
	edited := strings.Replace(snapshotFlowSrc, "https://x", "https://y", 1)

	mine := newCloudSnapshotDoc(t, snapshotFlowSrc)
	other := newCloudSnapshotDoc(t, snapshotFlowSrc)
	other.ID = "other-flow"
	if _, err := svc.SaveSource(ctx, mine, edited, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.SaveSource(ctx, other, edited, ""); err != nil {
		t.Fatal(err)
	}
	theirs := snapshotsOf(t, svc, other)[0].ID

	if _, err := svc.RestoreSourceSnapshot(ctx, mine, theirs); !errors.Is(err, ErrSnapshotNotFound) {
		t.Errorf("restoring another flow's snapshot = %v, want ErrSnapshotNotFound", err)
	}
	if _, err := svc.RestoreSourceSnapshot(ctx, mine, "no-such-snapshot"); !errors.Is(err, ErrSnapshotNotFound) {
		t.Errorf("restoring an unknown snapshot = %v, want ErrSnapshotNotFound", err)
	}
}

// The ring is 20 deep and byte-bounded per flow in the database; the newest entries stay.
func TestSnapshotStore_BoundsCountAndBytes(t *testing.T) {
	ctx := context.Background()
	backend := testutil.NewFakeBackend()
	svc := replica(backend)
	doc := newCloudSnapshotDoc(t, snapshotFlowSrc)
	if err := backend.SaveFlow(ctx, &storageif.FlowDocument{ID: doc.ID, Name: "f", OwnerID: doc.OwnerID, Content: []byte(`{}`)}); err != nil {
		t.Fatal(err)
	}

	for range maxFlowSnapshots + 5 {
		svc.pushSnapshot(ctx, doc, svc.snapshotCloudSourceText(doc, snapshotFlowSrc, "iter"))
	}
	if got := len(snapshotsOf(t, svc, doc)); got != maxFlowSnapshots {
		t.Fatalf("ring size = %d, want %d", got, maxFlowSnapshots)
	}

	// Bytes: a source over the per-flow bound is not kept and evicts nothing; others drop the oldest first.
	svc.snapshotLimits.MaxBytes = int64(3 * len(snapshotFlowSrc))
	before := snapshotsOf(t, svc, doc)
	svc.pushSnapshot(ctx, doc, svc.snapshotCloudSourceText(doc, strings.Repeat("x", 4*len(snapshotFlowSrc)), "too big"))
	if after := snapshotsOf(t, svc, doc); len(after) != len(before) || after[len(after)-1].ID != before[len(before)-1].ID {
		t.Errorf("a snapshot over the bound changed the ring: %d -> %d entries", len(before), len(after))
	}
	svc.pushSnapshot(ctx, doc, svc.snapshotCloudSourceText(doc, snapshotFlowSrc, "newest"))
	got := snapshotsOf(t, svc, doc)
	if len(got) != 3 || got[len(got)-1].Label != "newest" {
		t.Errorf("after the byte bound the ring is %d entries ending %q, want 3 ending 'newest'", len(got), got[len(got)-1].Label)
	}
}

// failingSnapshots is a store whose every call fails, as a database outage mid-request does.
type failingSnapshots struct{ storageif.FlowSnapshotStore }

func (failingSnapshots) PushFlowSnapshot(context.Context, *storageif.FlowSnapshot, storageif.FlowSnapshotLimits) error {
	return errors.New("snapshot store down")
}
func (failingSnapshots) ListFlowSnapshots(context.Context, string) ([]*storageif.FlowSnapshot, error) {
	return nil, errors.New("snapshot store down")
}

// The ring is an affordance: a snapshot that cannot be stored never fails the mutation it follows. Listing it reports
// the failure instead of an empty ring that looks like "nothing to undo".
func TestSnapshotStore_FailureDoesNotFailTheMutation(t *testing.T) {
	ctx := context.Background()
	backend := testutil.NewFakeBackend()
	svc := NewFlowService(&testutil.CountingNotifier{}, nil, NewLocalDocumentProvider(), backend, nil, nil, WithSnapshotStore(failingSnapshots{backend}))
	doc := newCloudSnapshotDoc(t, snapshotFlowSrc)

	if _, err := svc.SaveSource(ctx, doc, strings.Replace(snapshotFlowSrc, "https://x", "https://y", 1), ""); err != nil {
		t.Fatalf("SaveSource failed because the snapshot could not be stored: %v", err)
	}
	if _, err := svc.ListSourceSnapshots(ctx, doc); err == nil {
		t.Error("ListSourceSnapshots hid a store failure behind an empty list")
	}
}

// A desktop file flow keeps the in-memory ring even where a store exists (a file path is no row of flows), and a
// service without a store keeps the memory ring for cloud flows too.
func TestSnapshotStore_DesktopFilesAndNoStoreStayInMemory(t *testing.T) {
	ctx := context.Background()
	backend := testutil.NewFakeBackend()

	svc, doc := newDesktopFlowSvc(t)
	WithSnapshotStore(backend)(svc)
	if _, err := svc.SaveSource(ctx, doc, strings.Replace(snapshotFlowSrc, "https://x", "https://y", 1), ""); err != nil {
		t.Fatalf("SaveSource: %v", err)
	}
	if got := svc.snapshots.list(flowSnapshotKey(doc)); len(got) != 1 {
		t.Fatalf("desktop memory ring holds %d entries, want 1", len(got))
	}
	if got, _ := backend.ListFlowSnapshots(ctx, doc.ID); len(got) != 0 {
		t.Fatalf("a desktop file snapshot reached the database: %+v", got)
	}

	plain := newCloudSnapshotSvc(testutil.NewFakeBackend())
	cloud := newCloudSnapshotDoc(t, snapshotFlowSrc)
	if _, err := plain.SaveSource(ctx, cloud, strings.Replace(snapshotFlowSrc, "https://x", "https://y", 1), ""); err != nil {
		t.Fatal(err)
	}
	if got := len(plain.snapshots.list(flowSnapshotKey(cloud))); got != 1 {
		t.Fatalf("a service without a snapshot store holds %d entries in memory, want 1", got)
	}
}
