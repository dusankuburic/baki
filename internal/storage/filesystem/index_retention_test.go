package filesystem

import (
	"context"
	"strings"
	"testing"

	"pad-analyzer/internal/storage/interfaces"
)

// indexedSource returns the Source the metadata index retains for id.
func indexedSource(t *testing.T, lsb *LocalStorageBackend, id string) string {
	t.Helper()
	lsb.metaMu.RLock()
	defer lsb.metaMu.RUnlock()
	e, ok := lsb.flowMeta[id]
	if !ok {
		t.Fatalf("fixture rot: flow %s is not in the metadata index", id)
	}
	return e.header.Source
}

// TestListFlows_IndexDoesNotRetainSource guards the metadata index against
// pinning flow bodies: its headers cleared Content but kept Source, so after a
// single ListFlows every flow's full PAD text stayed in memory for the life of
// the backend — for the migration source (the only production user), every
// legacy flow in the data directory. Both writers of the index are covered: a
// fresh backend reading existing files (refreshMetaIndex — the migration
// source's path) and SaveFlow's in-place update. The full source must still
// come back from LoadFlow and from a non-MetadataOnly listing, which is what
// the migrator copies.
func TestListFlows_IndexDoesNotRetainSource(t *testing.T) {
	ctx := context.Background()
	source := strings.Repeat("Text.WriteToFile File: $'''C:\\out.txt''' TextToWrite: Line\n", 200)
	newFlow := func(id string) *interfaces.FlowDocument {
		return &interfaces.FlowDocument{
			ID: id, Name: "Legacy " + id, OwnerID: "u1",
			Content: []byte(`{"subflows":[]}`),
			Source:  source,
		}
	}

	checkListings := func(t *testing.T, lsb *LocalStorageBackend, id string) {
		t.Helper()
		headers, err := lsb.ListFlows(ctx, interfaces.FlowFilter{UserID: "u1", MetadataOnly: true})
		if err != nil {
			t.Fatalf("ListFlows MetadataOnly: %v", err)
		}
		if len(headers) != 1 || headers[0].ID != id {
			t.Fatalf("ListFlows MetadataOnly = %d flow(s), want exactly %s", len(headers), id)
		}
		if n := len(headers[0].Source); n != 0 {
			t.Errorf("MetadataOnly listing header carries %d bytes of Source, want none", n)
		}
		if n := len(indexedSource(t, lsb, id)); n != 0 {
			t.Errorf("metadata index retains %d bytes of Source for %s, pinning the whole flow text", n, id)
		}

		full, err := lsb.LoadFlow(ctx, id)
		if err != nil {
			t.Fatalf("LoadFlow: %v", err)
		}
		if full.Source != source {
			t.Errorf("LoadFlow Source = %d bytes, want the full %d", len(full.Source), len(source))
		}
		hydrated, err := lsb.ListFlows(ctx, interfaces.FlowFilter{AllFlows: true, SortBy: interfaces.FlowSortIDAsc})
		if err != nil {
			t.Fatalf("ListFlows full: %v", err)
		}
		if len(hydrated) != 1 || hydrated[0].Source != source {
			t.Errorf("full listing lost the Source the migrator copies")
		}
	}

	t.Run("fresh backend over existing files", func(t *testing.T) {
		dir := t.TempDir()
		writer, err := NewLocalStorageBackend(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := writer.SaveFlow(ctx, newFlow("legacy-1")); err != nil {
			t.Fatalf("SaveFlow: %v", err)
		}

		// A second backend over the same directory has an empty index, so the
		// listing builds every header from the file — as the migration source
		// does over a legacy data dir.
		src, err := NewLocalStorageBackend(dir)
		if err != nil {
			t.Fatal(err)
		}
		checkListings(t, src, "legacy-1")
	})

	t.Run("SaveFlow updates the index in place", func(t *testing.T) {
		lsb, err := NewLocalStorageBackend(t.TempDir())
		if err != nil {
			t.Fatal(err)
		}
		if err := lsb.SaveFlow(ctx, newFlow("saved-1")); err != nil {
			t.Fatalf("SaveFlow: %v", err)
		}
		// Checked before any listing, so this is SaveFlow's own entry and
		// not one refreshMetaIndex rebuilt from the file.
		if n := len(indexedSource(t, lsb, "saved-1")); n != 0 {
			t.Errorf("SaveFlow's index entry retains %d bytes of Source", n)
		}
		checkListings(t, lsb, "saved-1")
	})
}
