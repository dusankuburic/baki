package contract

import (
	"context"
	"encoding/json"
	"slices"
	"testing"
	"time"

	"pad-analyzer/internal/storage/interfaces"
)

// runKeysetPageScenarios holds the paged lists to their keyset cursors (interfaces.PageCursor): a cursor continues the
// list's order exactly where the page before ended, ties on the timestamp broken by the id, beside the offsets the
// older clients send. The library page also reports whether more flows follow and a total counted up to a cap; the
// admin user search matches a prefix for a term without a run of three letters or digits.
func runKeysetPageScenarios(ctx context.Context, b interfaces.StorageBackend, runID string, run func(name string, fn func(t *testing.T))) {
	// Whole seconds: both backends keep them exactly (Postgres stores microseconds).
	t0 := time.Now().UTC().Truncate(time.Second).Add(-time.Hour)
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }

	run("ListFlowsPage_pages_by_offset_and_keyset_with_a_capped_total", func(t *testing.T) {
		owner := "contract-kp-owner-" + runID
		id := func(n string) string { return "contract-kp-" + runID + "-" + n }
		// Newest first: a, then b and c (same time, id order), d, e.
		for _, f := range []struct {
			name string
			upd  int
		}{{"a", 5}, {"b", 4}, {"c", 4}, {"d", 2}, {"e", 1}} {
			doc := &interfaces.FlowDocument{ID: id(f.name), Name: f.name, Content: json.RawMessage(`{}`), OwnerID: owner,
				CreatedAt: at(0), UpdatedAt: at(f.upd)}
			if err := b.ImportFlow(ctx, doc); err != nil {
				t.Fatalf("ImportFlow %s: %v", f.name, err)
			}
			fid := doc.ID
			t.Cleanup(func() { _ = b.DeleteFlow(context.Background(), fid) })
		}
		page := func(f interfaces.FlowFilter, totalCap int) *interfaces.FlowPage {
			t.Helper()
			p, err := b.ListFlowsPage(ctx, f, totalCap)
			if err != nil {
				t.Fatalf("ListFlowsPage(%+v, %d): %v", f, totalCap, err)
			}
			return p
		}
		expect := func(what string, p *interfaces.FlowPage, ids []string, hasMore bool, total int, capped bool) {
			t.Helper()
			if got := flowIDs(p.Docs); !slices.Equal(got, ids) || p.HasMore != hasMore || p.Total != total || p.TotalCapped != capped {
				t.Errorf("%s: %v hasMore=%v total=%d capped=%v; want %v %v %d %v", what, got, p.HasMore, p.Total, p.TotalCapped, ids, hasMore, total, capped)
			}
		}
		cursor := func(p *interfaces.FlowPage) *interfaces.PageCursor {
			last := p.Docs[len(p.Docs)-1]
			return &interfaces.PageCursor{At: last.UpdatedAt, ID: last.ID}
		}

		f := interfaces.FlowFilter{UserID: owner, SortBy: interfaces.FlowSortUpdatedDesc, Limit: 2, MetadataOnly: true}
		first := page(f, 0)
		expect("first page", first, []string{id("a"), id("b")}, true, 5, false)
		if first.Docs[0].Content != nil {
			t.Errorf("MetadataOnly page carries content")
		}
		f.After = cursor(first)
		second := page(f, 0)
		expect("after b", second, []string{id("c"), id("d")}, true, 5, false)
		f.After = cursor(second)
		expect("after d", page(f, 0), []string{id("e")}, false, 5, false)
		// A cursor wins over an offset; ListFlows and CountFlows see the same rows after it.
		f.After, f.Offset = cursor(first), 3
		expect("after b, offset ignored", page(f, 0), []string{id("c"), id("d")}, true, 5, false)
		if docs, err := b.ListFlows(ctx, f); err != nil || !slices.Equal(flowIDs(docs), []string{id("c"), id("d")}) {
			t.Errorf("ListFlows after b = %v, %v", flowIDs(docs), err)
		}
		if n, err := b.CountFlows(ctx, f); err != nil || n != 3 {
			t.Errorf("CountFlows after b = %d, %v; want 3", n, err)
		}

		// Offsets, as the older clients page.
		f.After = nil
		for _, tc := range []struct {
			offset  int
			ids     []string
			hasMore bool
		}{{2, []string{id("c"), id("d")}, true}, {4, []string{id("e")}, false}, {10, []string{}, false}} {
			f.Offset = tc.offset
			expect("offset", page(f, 0), tc.ids, tc.hasMore, 5, false)
		}
		// The total stops at the cap: more than 3 match, exactly 5 match.
		f.Offset = 0
		expect("cap 3", page(f, 3), []string{id("a"), id("b")}, true, 3, true)
		expect("cap 4", page(f, 4), []string{id("a"), id("b")}, true, 4, true)
		expect("cap 5", page(f, 5), []string{id("a"), id("b")}, true, 5, false)
		f.Offset = 4
		expect("cap 3, last page", page(f, 3), []string{id("e")}, false, 3, true)

		// Oldest first: after d come b and c (same time, id order), then a.
		f = interfaces.FlowFilter{UserID: owner, SortBy: interfaces.FlowSortUpdatedAsc, Limit: 2, MetadataOnly: true,
			After: &interfaces.PageCursor{At: at(2), ID: id("d")}}
		expect("ascending after d", page(f, 0), []string{id("b"), id("c")}, true, 5, false)
		f.After = &interfaces.PageCursor{At: at(4), ID: id("b")}
		expect("ascending after b", page(f, 0), []string{id("c"), id("a")}, false, 5, false)
		// The cursor is a position in the updated-at order: with another sort it matches nothing.
		f.SortBy = interfaces.FlowSortNameAsc
		expect("name sort with a cursor", page(f, 0), []string{}, false, 5, false)

		f = interfaces.FlowFilter{UserID: owner + "-nobody", Limit: 2, MetadataOnly: true}
		for _, offset := range []int{0, 10} {
			f.Offset = offset
			expect("no match", page(f, 0), []string{}, false, 0, false)
		}
	})

	run("QueryUsers_pages_by_keyset_and_matches_a_short_needle_as_a_prefix", func(t *testing.T) {
		mark := "kpu-" + runID
		id := func(n string) string { return "contract-kpu-" + runID + "-" + n }
		for _, u := range []struct {
			name, display string
			created       int
		}{{"a", "q~ first " + mark, 5}, {"b", "x q~ inside " + mark, 4}, {"c", "Plain " + mark, 4}, {"d", "Plain " + mark, 2}} {
			user := &interfaces.User{ID: id(u.name), Email: id(u.name) + "@example.com", Password: "hash", DisplayName: u.display, CreatedAt: at(u.created)}
			if err := b.SaveUser(ctx, user); err != nil {
				t.Fatalf("SaveUser %s: %v", u.name, err)
			}
			uid := user.ID
			t.Cleanup(func() { _ = b.DeleteUser(context.Background(), uid) })
		}
		ids := func(users []*interfaces.User) []string {
			out := []string{}
			for _, u := range users {
				out = append(out, u.ID)
			}
			return out
		}
		q := interfaces.UserQuery{Q: mark, Limit: 2}
		first, total, err := b.QueryUsers(ctx, q)
		if err != nil || !slices.Equal(ids(first), []string{id("a"), id("b")}) || total != 4 {
			t.Fatalf("first page = %v of %d, %v; want a, b of 4", ids(first), total, err)
		}
		q.After = &interfaces.PageCursor{At: first[1].CreatedAt, ID: first[1].ID}
		q.Offset = 3 // ignored with a cursor
		second, total, err := b.QueryUsers(ctx, q)
		if err != nil || !slices.Equal(ids(second), []string{id("c"), id("d")}) || total != 4 {
			t.Errorf("after b = %v of %d, %v; want c, d of 4 (the total counts every match)", ids(second), total, err)
		}
		q.After = &interfaces.PageCursor{At: at(2), ID: id("d")}
		if rest, total, err := b.QueryUsers(ctx, q); err != nil || rest == nil || len(rest) != 0 || total != 4 {
			t.Errorf("after d = %v of %d, %v; want a non-nil empty page of 4", ids(rest), total, err)
		}

		// Without a run of three letters or digits the needle matches the start of the email or the display name
		// only; LIKE's wildcards in it are literal.
		mine := func(users []*interfaces.User) []string {
			out := []string{}
			for _, u := range users {
				if slices.Contains([]string{id("a"), id("b"), id("c"), id("d")}, u.ID) {
					out = append(out, u.ID)
				}
			}
			return out
		}
		for needle, want := range map[string][]string{"q~": {id("a")}, "Q~ ": {id("a")}, "q_": {}, "q%": {}} {
			got, _, err := b.QueryUsers(ctx, interfaces.UserQuery{Q: needle, Limit: 500})
			if err != nil || !slices.Equal(mine(got), want) {
				t.Errorf("Q=%q: %v, %v; want %v", needle, mine(got), err, want)
			}
		}
		if got, _, err := b.QueryUsers(ctx, interfaces.UserQuery{Q: "q~ inside", Limit: 500}); err != nil || !slices.Equal(mine(got), []string{id("b")}) {
			t.Errorf("a longer needle is a substring: %v, %v; want b", mine(got), err)
		}
	})

	run("ListAuditEvents_pages_by_keyset", func(t *testing.T) {
		user := "contract-kpa-" + runID
		id := func(n string) string { return "contract-kpa-" + runID + "-" + n }
		for _, e := range []*interfaces.AuditEvent{
			{ID: id("e1"), UserID: user, Action: "flow.save", ResourceType: "flow", ResourceID: "f", CreatedAt: at(5)},
			{ID: id("e2"), UserID: user, Action: "flow.save", ResourceType: "flow", ResourceID: "f", CreatedAt: at(4)},
			{ID: id("e3"), UserID: user, Action: "flow.save", ResourceType: "flow", ResourceID: "f", CreatedAt: at(4)},
			// A failed sign-in names the user as its subject only.
			{ID: id("s1"), UserID: "", Action: "auth.login_failure", ResourceType: "user", ResourceID: user, CreatedAt: at(3)},
			{ID: id("e4"), UserID: user, Action: "flow.save", ResourceType: "flow", ResourceID: "f", CreatedAt: at(2)},
		} {
			if err := b.SaveAuditEvent(ctx, e); err != nil {
				t.Fatalf("SaveAuditEvent %s: %v", e.ID, err)
			}
		}
		walk := func(f interfaces.AuditFilter) [][]string {
			t.Helper()
			var pages [][]string
			for range 10 {
				evs, err := b.ListAuditEvents(ctx, f)
				if err != nil {
					t.Fatalf("ListAuditEvents(%+v): %v", f, err)
				}
				if len(evs) == 0 {
					return pages
				}
				var page []string
				for _, e := range evs {
					page = append(page, e.ID)
				}
				pages = append(pages, page)
				last := evs[len(evs)-1]
				f.After, f.Offset = &interfaces.PageCursor{At: last.CreatedAt, ID: last.ID}, 1 // the offset is ignored
			}
			t.Fatal("the walk does not end")
			return nil
		}
		if got, want := walk(interfaces.AuditFilter{UserID: user, Limit: 2}), [][]string{{id("e1"), id("e2")}, {id("e3"), id("e4")}}; !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("actor walk = %v, want %v", got, want)
		}
		if got, want := walk(interfaces.AuditFilter{UserID: user, IncludeSubject: true, Limit: 2}), [][]string{{id("e1"), id("e2")}, {id("e3"), id("s1")}, {id("e4")}}; !slices.EqualFunc(got, want, slices.Equal) {
			t.Errorf("actor and subject walk = %v, want %v", got, want)
		}
	})
}
