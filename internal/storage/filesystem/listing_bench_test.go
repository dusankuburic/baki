package filesystem

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pad-analyzer/internal/storage/contract"
	"pad-analyzer/internal/storage/interfaces"
)

// legacyDirWithFlows lays out n flows with small content as an old build wrote them.
func legacyDirWithFlows(t testing.TB, n int) *contract.LegacyDir {
	t.Helper()
	d := contract.NewLegacyDir(t)
	blocks := make([]string, 40)
	for i := range blocks {
		blocks[i] = fmt.Sprintf(`{"id":"b%d","name":"Block %d","type":"action","rawType":"Text.WriteToFile"}`, i, i)
	}
	content := json.RawMessage(`{"subflows":[{"blocks":[` + strings.Join(blocks, ",") + `]}]}`)
	for i := range n {
		d.Flow(&interfaces.FlowDocument{ID: fmt.Sprintf("flow-%04d", i), Name: fmt.Sprintf("Flow %d", i), OwnerID: "u1", Content: content})
	}
	return d
}

// BenchmarkLegacySource_ListFlowIDs measures one page of the migrator's walk: a directory read and a sort, no flow
// file opened.
func BenchmarkLegacySource_ListFlowIDs(b *testing.B) {
	src, err := OpenLegacySource(legacyDirWithFlows(b, 300).Root)
	if err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		if _, err := src.ListFlowIDs(ctx, "flow-0100", 50); err != nil {
			b.Fatal(err)
		}
	}
}

// TestLegacySource_SeesExternalEdits: the source keeps no index or cache, so a file an old build rewrites, adds or
// removes after the source opened is read as it is now.
func TestLegacySource_SeesExternalEdits(t *testing.T) {
	d := legacyDirWithFlows(t, 3)
	src, err := OpenLegacySource(d.Root)
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	if _, err := src.LoadFlow(ctx, "flow-0001"); err != nil {
		t.Fatal(err)
	}

	d.Flow(&interfaces.FlowDocument{ID: "flow-0001", Name: "Renamed Externally", OwnerID: "u1"})
	d.Flow(&interfaces.FlowDocument{ID: "flow-0003", Name: "Added Externally", OwnerID: "u1"})
	if err := os.Remove(filepath.Join(d.Root, "flows", "flow-0000.json")); err != nil {
		t.Fatal(err)
	}

	ids, err := src.ListFlowIDs(ctx, "", 10)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"flow-0001", "flow-0002", "flow-0003"}; !slices.Equal(ids, want) {
		t.Errorf("ListFlowIDs after external edits = %v, want %v", ids, want)
	}
	got, err := src.LoadFlow(ctx, "flow-0001")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "Renamed Externally" {
		t.Errorf("LoadFlow after an external rewrite: name = %q", got.Name)
	}
}
