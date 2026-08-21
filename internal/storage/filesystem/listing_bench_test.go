package filesystem

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// seedFlows writes n flows with small content into the backend.
func seedFlows(t testing.TB, lsb *LocalStorageBackend, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		doc := &interfaces.FlowDocument{
			ID:        fmt.Sprintf("flow-%04d", i),
			Name:      fmt.Sprintf("Flow %d", i),
			OwnerID:   "u1",
			UpdatedAt: time.Now(),
			Content:   []byte(`{"subflows":[{"blocks":[` + repeatBlocks(40) + `]}]}`),
		}
		if err := lsb.SaveFlow(context.Background(), doc); err != nil {
			t.Fatalf("seed %d: %v", i, err)
		}
	}
}

func repeatBlocks(n int) string {
	s := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			s += ","
		}
		s += fmt.Sprintf(`{"id":"b%d","name":"Block %d","type":"action","rawType":"Text.WriteToFile"}`, i, i)
	}
	return s
}

// BenchmarkListFlows_MetadataOnly measures the library-listing path: with the
// metadata index this is a Stat sweep + map walk; previously it read and fully
// JSON-parsed every stored flow per listing (and again for CountFlows).
func BenchmarkListFlows_MetadataOnly(b *testing.B) {
	lsb, err := NewLocalStorageBackend(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	seedFlows(b, lsb, 300)
	ctx := context.Background()
	filter := interfaces.FlowFilter{UserID: "u1", MetadataOnly: true, Limit: 50}
	// First listing populates the index; the benchmark measures steady state.
	if _, err := lsb.ListFlows(ctx, filter); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := lsb.ListFlows(ctx, filter); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkCountFlows measures the count path (index walk vs the previous
// read+parse-every-file scan).
func BenchmarkCountFlows(b *testing.B) {
	lsb, err := NewLocalStorageBackend(b.TempDir())
	if err != nil {
		b.Fatal(err)
	}
	seedFlows(b, lsb, 300)
	ctx := context.Background()
	if _, err := lsb.CountFlows(ctx, interfaces.FlowFilter{UserID: "u1"}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := lsb.CountFlows(ctx, interfaces.FlowFilter{UserID: "u1"}); err != nil {
			b.Fatal(err)
		}
	}
}

// TestListFlows_IndexConsistencyAfterExternalEdit proves the Stat validation
// catches a file modified outside the backend (mtime bump) and re-reads it,
// so listings never serve stale metadata.
func TestListFlows_IndexConsistencyAfterExternalEdit(t *testing.T) {
	lsb, err := NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	seedFlows(t, lsb, 3)
	ctx := context.Background()

	// Prime the index, then externally rewrite one flow's file with a new
	// name + future mtime (Stat must see the change and re-read).
	flows, err := lsb.ListFlows(ctx, interfaces.FlowFilter{UserID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(flows) != 3 {
		t.Fatalf("expected 3 seeded flows, got %d", len(flows))
	}

	dir := filepath.Join(lsb.dataDir, "flows")
	path := filepath.Join(dir, "flow-0001.json")
	doc := &interfaces.FlowDocument{ID: "flow-0001", Name: "Renamed Externally", OwnerID: "u1"}
	data, mErr := json.Marshal(doc)
	if mErr != nil {
		t.Fatal(mErr)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(2 * time.Second)
	if err := os.Chtimes(path, future, future); err != nil {
		t.Fatal(err)
	}

	flows, err = lsb.ListFlows(ctx, interfaces.FlowFilter{UserID: "u1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, f := range flows {
		if f.ID == "flow-0001" && f.Name != "Renamed Externally" {
			t.Errorf("stale index served after external edit: name=%q", f.Name)
		}
	}
}
