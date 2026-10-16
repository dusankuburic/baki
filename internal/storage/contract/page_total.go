package contract

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// runPageTotalScenarios holds a library keyset page to the total its cursor carries (PageCursor.Counted): the first
// page counted it, and counting again on every page read the caller's whole visible set a second time. A cursor that
// carries none leaves the page to count, as before. A cursor's Skip passes over that many flows after its position.
func runPageTotalScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(name string, fn func(t *testing.T))) {
	run("ListFlowsPage_keyset_page_reports_the_total_its_cursor_carries", func(t *testing.T) {
		owner := "contract-pt-owner-" + runID
		id := func(n string) string { return "contract-pt-" + runID + "-" + n }
		t0 := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
		for i, n := range []string{"c", "b", "a"} { // a newest
			doc := &interfaces.FlowDocument{ID: id(n), Name: n, Content: json.RawMessage(`{}`), OwnerID: owner,
				CreatedAt: t0, UpdatedAt: t0.Add(time.Duration(i) * time.Second)}
			if err := b.ImportFlow(ctx, doc); err != nil {
				t.Fatalf("ImportFlow %s: %v", n, err)
			}
			fid := doc.ID
			t.Cleanup(func() { _ = b.DeleteFlow(context.Background(), fid) })
		}
		f := interfaces.FlowFilter{UserID: owner, SortBy: interfaces.FlowSortUpdatedDesc, Limit: 1, MetadataOnly: true}
		first, err := b.ListFlowsPage(ctx, f, 0)
		if err != nil || first.Total != 3 || !first.HasMore {
			t.Fatalf("first page = %+v, %v; want total 3 and more", first, err)
		}
		last := first.Docs[len(first.Docs)-1]
		for _, tc := range []struct {
			what     string
			after    interfaces.PageCursor
			totalCap int
			total    int
			capped   bool
		}{
			{"a counted cursor", interfaces.PageCursor{Counted: true, Total: 42}, 0, 42, false},
			{"a counted, capped cursor", interfaces.PageCursor{Counted: true, Total: 100, TotalCapped: true}, 100, 100, true},
			{"a cursor without a total", interfaces.PageCursor{}, 0, 3, false},
		} {
			after := tc.after
			after.At, after.ID = last.UpdatedAt, last.ID
			f.After = &after
			p, err := b.ListFlowsPage(ctx, f, tc.totalCap)
			if err != nil {
				t.Fatalf("%s: %v", tc.what, err)
			}
			if !slices.Equal(flowIDs(p.Docs), []string{id("b")}) || !p.HasMore || p.Total != tc.total || p.TotalCapped != tc.capped {
				t.Errorf("%s: %v more=%v total=%d capped=%v; want [b] more, %d %v", tc.what, flowIDs(p.Docs), p.HasMore, p.Total, p.TotalCapped, tc.total, tc.capped)
			}
		}
	})

	run("ListFlowsPage_keyset_page_passes_over_its_cursors_skip", func(t *testing.T) {
		owner := "contract-ps-owner-" + runID
		id := func(n string) string { return "contract-ps-" + runID + "-" + n }
		t0 := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
		for i, n := range []string{"e", "d", "c", "b", "a"} { // a newest
			doc := &interfaces.FlowDocument{ID: id(n), Name: n, Content: json.RawMessage(`{}`), OwnerID: owner,
				CreatedAt: t0, UpdatedAt: t0.Add(time.Duration(i) * time.Second)}
			if err := b.ImportFlow(ctx, doc); err != nil {
				t.Fatalf("ImportFlow %s: %v", n, err)
			}
			fid := doc.ID
			t.Cleanup(func() { _ = b.DeleteFlow(context.Background(), fid) })
		}
		at := func(n string) time.Time { return t0.Add(time.Duration(4-int(n[0]-'a')) * time.Second) }
		for _, tc := range []struct {
			what    string
			sort    interfaces.FlowSort
			after   string
			skip    int
			want    []string
			hasMore bool
		}{
			{"newest first, after a, passing over b", interfaces.FlowSortUpdatedDesc, "a", 1, []string{id("c"), id("d")}, true},
			{"newest first, after a, passing over b to d", interfaces.FlowSortUpdatedDesc, "a", 3, []string{id("e")}, false},
			{"newest first, passing over the rest", interfaces.FlowSortUpdatedDesc, "a", 9, []string{}, false},
			{"oldest first, after e, passing over d", interfaces.FlowSortUpdatedAsc, "e", 1, []string{id("c"), id("b")}, true},
		} {
			f := interfaces.FlowFilter{UserID: owner, SortBy: tc.sort, Limit: 2, MetadataOnly: true, Offset: 1, // ignored
				After: &interfaces.PageCursor{At: at(tc.after), ID: id(tc.after), Skip: tc.skip, Counted: true, Total: 5}}
			p, err := b.ListFlowsPage(ctx, f, 0)
			if err != nil {
				t.Fatalf("%s: %v", tc.what, err)
			}
			if !slices.Equal(flowIDs(p.Docs), tc.want) || p.HasMore != tc.hasMore || p.Total != 5 {
				t.Errorf("%s: %v more=%v total=%d; want %v more=%v, 5", tc.what, flowIDs(p.Docs), p.HasMore, p.Total, tc.want, tc.hasMore)
			}
			if docs, err := b.ListFlows(ctx, f); err != nil || !slices.Equal(flowIDs(docs), tc.want) {
				t.Errorf("%s: ListFlows = %v, %v; want %v", tc.what, flowIDs(docs), err, tc.want)
			}
		}
		// CountFlows counts every flow after the position, the passed-over ones too.
		f := interfaces.FlowFilter{UserID: owner, SortBy: interfaces.FlowSortUpdatedDesc, After: &interfaces.PageCursor{At: at("a"), ID: id("a"), Skip: 2}}
		if n, err := b.CountFlows(ctx, f); err != nil || n != 4 {
			t.Errorf("CountFlows after a with a skip = %d, %v; want 4", n, err)
		}
	})
}
