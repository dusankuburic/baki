package filesystem

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"pad-analyzer/internal/storage/contract"
	"pad-analyzer/internal/storage/interfaces"
)

// walkFlowIDs is the loop in migration.Migrate: keyset pages of pageSize until a short page. between runs after
// each page, for a test that changes the directory mid-walk.
func walkFlowIDs(t *testing.T, src *LegacySource, pageSize int, between func(page int)) map[string]int {
	t.Helper()
	seen := map[string]int{}
	after := ""
	for page := 0; ; page++ {
		if page > 1000 {
			t.Fatal("the walk does not end")
		}
		ids, err := src.ListFlowIDs(context.Background(), after, pageSize)
		if err != nil {
			t.Fatalf("ListFlowIDs(after %q): %v", after, err)
		}
		for _, id := range ids {
			seen[id]++
		}
		if len(ids) < pageSize {
			return seen
		}
		after = ids[len(ids)-1]
		if between != nil {
			between(page)
		}
	}
}

// TestLegacySource_WalkVisitsEveryFlowOnce pins the property the migrator's copy depends on: walking ListFlowIDs page
// by page visits every flow exactly once. Flows that tie on name and timestamps are the ordinary case in an old data
// directory (the old backend wrote whatever UpdatedAt the caller supplied, often none); the walk's key is the file
// name, so ties cannot reorder pages.
func TestLegacySource_WalkVisitsEveryFlowOnce(t *testing.T) {
	const total, pageSize = 40, 10
	for _, tc := range []struct {
		name  string
		stamp func(i int) time.Time
	}{
		{"all tied on the zero time", func(int) time.Time { return time.Time{} }},
		{"all tied on one timestamp (bulk import)", func(int) time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }},
		{"distinct timestamps", func(i int) time.Time {
			return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC).Add(time.Duration(i) * time.Minute)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := contract.NewLegacyDir(t)
			for i := range total {
				id := fmt.Sprintf("flow-%02d", i)
				d.Flow(&interfaces.FlowDocument{ID: id, Name: "same name", Content: json.RawMessage(`{}`), UpdatedAt: tc.stamp(i)})
			}
			src, err := OpenLegacySource(d.Root)
			if err != nil {
				t.Fatal(err)
			}
			seen := walkFlowIDs(t, src, pageSize, nil)
			if len(seen) != total {
				t.Errorf("the walk visited %d of %d flows", len(seen), total)
			}
			for id, n := range seen {
				if n != 1 {
					t.Errorf("%s visited %d times", id, n)
				}
			}
		})
	}
}

// TestLegacySource_WalkSurvivesChangesMidWalk: an old build may still be serving the directory while the migration
// runs. With LIMIT/OFFSET paging, removing a flow the walk already passed shifted every later page and skipped a
// flow; with the keyset, a removal or an addition only affects that flow.
func TestLegacySource_WalkSurvivesChangesMidWalk(t *testing.T) {
	const total, pageSize = 40, 10
	d := contract.NewLegacyDir(t)
	for i := range total {
		id := fmt.Sprintf("flow-%02d", i)
		d.Flow(&interfaces.FlowDocument{ID: id, Name: id, Content: json.RawMessage(`{}`)})
	}
	src, err := OpenLegacySource(d.Root)
	if err != nil {
		t.Fatal(err)
	}
	remove := func(id string) {
		if err := os.Remove(filepath.Join(d.Root, "flows", id+".json")); err != nil {
			t.Fatal(err)
		}
	}
	seen := walkFlowIDs(t, src, pageSize, func(page int) {
		if page != 0 {
			return
		}
		remove("flow-03") // already visited
		remove("flow-35") // not yet visited
		d.Flow(&interfaces.FlowDocument{ID: "flow-00a", Name: "behind the cursor", Content: json.RawMessage(`{}`)})
		d.Flow(&interfaces.FlowDocument{ID: "flow-99", Name: "ahead of the cursor", Content: json.RawMessage(`{}`)})
	})

	for i := range total {
		id := fmt.Sprintf("flow-%02d", i)
		want := 1
		if id == "flow-35" {
			want = 0
		}
		if seen[id] != want {
			t.Errorf("%s visited %d times, want %d", id, seen[id], want)
		}
	}
	if seen["flow-99"] != 1 {
		t.Errorf("a flow added ahead of the cursor was visited %d times, want 1", seen["flow-99"])
	}
	if seen["flow-00a"] != 0 {
		t.Errorf("a flow added behind the cursor was visited %d times, want 0 (a rerun copies it)", seen["flow-00a"])
	}
}

// TestLegacySource_ListingIsDeterministic: identical calls return identical pages, whatever order the directory
// returns its entries in. Without it no paged walk can be correct.
func TestLegacySource_ListingIsDeterministic(t *testing.T) {
	d := contract.NewLegacyDir(t)
	for i := range 40 {
		d.Flow(&interfaces.FlowDocument{ID: fmt.Sprintf("flow-%02d", i), Name: "same name", Content: json.RawMessage(`{}`)})
	}
	src, err := OpenLegacySource(d.Root)
	if err != nil {
		t.Fatal(err)
	}
	list := func() []string {
		ids, err := src.ListFlowIDs(context.Background(), "flow-05", 20)
		if err != nil {
			t.Fatalf("ListFlowIDs: %v", err)
		}
		return ids
	}
	first := list()
	if len(first) != 20 || first[0] != "flow-06" {
		t.Fatalf("ListFlowIDs(after flow-05, 20) = %v", first)
	}
	for i := range 20 {
		if got := list(); !slices.Equal(got, first) {
			t.Fatalf("call %d returned %v, the first returned %v", i, got, first)
		}
	}
}
