package filesystem

import (
	"context"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"pad-analyzer/internal/migration"
	"pad-analyzer/internal/storage/contract"
	"pad-analyzer/internal/storage/interfaces"
)

// TestLegacySource_Contract runs the migrator's read-only source contract against raw files laid out as old builds
// wrote them.
func TestLegacySource_Contract(t *testing.T) {
	contract.RunLegacySourceSuite(t, func(t *testing.T, dataDir string) migration.LegacySource {
		src, err := OpenLegacySource(dataDir)
		if err != nil {
			t.Fatalf("OpenLegacySource: %v", err)
		}
		return src
	})
}

// TestOpenLegacySource_DoesNotCreateTheDirectory: the old backend created a missing data directory, which turned a
// mistyped PAD_DATA_DIR into an empty migration that reported success.
func TestOpenLegacySource_DoesNotCreateTheDirectory(t *testing.T) {
	missing := filepath.Join(t.TempDir(), "no-such-dir")
	if _, err := OpenLegacySource(missing); err == nil {
		t.Fatal("OpenLegacySource on a missing directory should fail")
	}
	if _, err := os.Stat(missing); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("OpenLegacySource created %s (stat: %v)", missing, err)
	}

	file := filepath.Join(t.TempDir(), "settings.json")
	if err := os.WriteFile(file, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := OpenLegacySource(file); err == nil {
		t.Error("OpenLegacySource on a file should fail")
	}
}

// TestLegacySource_ListsEveryOwnersFlows: the migrator copies the whole directory, so the listing is not scoped to
// a caller (it was ListFlows with AllFlows): personal flows of any owner, org flows and ownerless flows are listed.
func TestLegacySource_ListsEveryOwnersFlows(t *testing.T) {
	d := contract.NewLegacyDir(t)
	for _, f := range []*interfaces.FlowDocument{
		{ID: "flow-a", Name: "A", OwnerID: "user-1"},
		{ID: "flow-b", Name: "B", OwnerID: "user-2"},
		{ID: "flow-c", Name: "C", OwnerID: "user-1", OrganizationID: "org-1"},
		{ID: "flow-d", Name: "D"},
	} {
		f.Content = json.RawMessage(`{}`)
		d.Flow(f)
	}
	src, err := OpenLegacySource(d.Root)
	if err != nil {
		t.Fatal(err)
	}
	ids, err := src.ListFlowIDs(context.Background(), "", 100)
	if err != nil {
		t.Fatal(err)
	}
	if want := []string{"flow-a", "flow-b", "flow-c", "flow-d"}; !slices.Equal(ids, want) {
		t.Errorf("ListFlowIDs = %v, want %v", ids, want)
	}
}

// TestLegacySource_LoadFlowReturnsTheFullSource: the listing carries IDs only, so nothing of a flow's body stays in
// memory between pages, and LoadFlow returns the full PAD text the old build stored.
func TestLegacySource_LoadFlowReturnsTheFullSource(t *testing.T) {
	source := strings.Repeat("Text.WriteToFile File: $'''C:\\out.txt''' TextToWrite: Line\n", 200)
	d := contract.NewLegacyDir(t)
	d.Flow(&interfaces.FlowDocument{ID: "legacy-1", Name: "Legacy", OwnerID: "u1", Content: json.RawMessage(`{"subflows":[]}`), Source: source})
	src, err := OpenLegacySource(d.Root)
	if err != nil {
		t.Fatal(err)
	}
	got, err := src.LoadFlow(context.Background(), "legacy-1")
	if err != nil {
		t.Fatalf("LoadFlow: %v", err)
	}
	if got.Source != source {
		t.Errorf("LoadFlow Source = %d bytes, want the full %d", len(got.Source), len(source))
	}
}
