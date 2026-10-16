package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"testing"
	"time"

	storageif "pad-analyzer/internal/storage/interfaces"
	"pad-analyzer/internal/testutil"
)

// longFlowID is an id a legacy import can carry (it keeps the data directory's file name) and a cursor cannot: over
// maxListCursorIDLen bytes.
func longFlowID(i int) string {
	return fmt.Sprintf("legacy-%05d-", i) + strings.Repeat("Ж", 130)
}

// seedOrderedFlows imports n flows of owner, newest first in index order (flow i updated i seconds before flow 0), the
// indices in long with an id too long for a cursor, and returns their ids in that order.
func seedOrderedFlows(t *testing.T, b storageif.StorageBackend, owner string, n int, long map[int]bool) []string {
	t.Helper()
	base := time.Now().UTC().Truncate(time.Second)
	ids := make([]string, n)
	for i := range n {
		id := fmt.Sprintf("flow-%05d", i)
		if long[i] {
			id = longFlowID(i)
		}
		doc := &storageif.FlowDocument{ID: id, Name: "test", OwnerID: owner, Content: json.RawMessage(`{}`),
			CreatedAt: base, UpdatedAt: base.Add(-time.Duration(i) * time.Second)}
		if err := b.ImportFlow(context.Background(), doc); err != nil {
			t.Fatalf("import %s: %v", id, err)
		}
		ids[i] = id
	}
	return ids
}

// TestLibraryList_CursorWalkPassesLongIDs: a page whose last flows have ids too long for a cursor gives the cursor
// after its last flow that has one, and the next page passes over the rest; a page of nothing but such ids continues
// from the cursor it was reached by. A client following nextCursor alone (no offset) reaches every flow once, in both
// updated-at orders: it used to get hasMore without a cursor and, paging on by the echoed offset 0, start over.
func TestLibraryList_CursorWalkPassesLongIDs(t *testing.T) {
	backend := testutil.NewFakeBackend()
	rt := newTestRouter(backend, true)
	ids := seedOrderedFlows(t, backend, "alice", 20, map[int]bool{2: true, 6: true, 7: true, 8: true, 9: true, 14: true, 19: true})
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")
	asc := slices.Clone(ids)
	slices.Reverse(asc)
	for sort, want := range map[string][]string{"updated_desc": ids, "updated_asc": asc} {
		var walked []string
		for _, p := range walkPages(t, rt, bearer, "/api/library/?limit=3&sort="+sort) {
			walked = append(walked, p.ids()...)
		}
		if !slices.Equal(walked, want) {
			t.Errorf("%s: the cursor walk read %d flows, %d distinct, of %d; want each once, in order", sort, len(walked), len(slices.Compact(slices.Sorted(slices.Values(walked)))), len(want))
		}
	}
	// A page reached by offset whose flows all have long ids gives no cursor: its offset is exact, the client pages on
	// by offset.
	var p cursorPage
	decodeJSON(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/library/?limit=3&offset=6", bearer, nil), &p)
	if !slices.Equal(p.ids(), ids[6:9]) || !p.HasMore || p.NextCursor != "" {
		t.Errorf("offset 6 = %d flows, more %v, cursor %v; want flows 6-8, more, no cursor", len(p.ids()), p.HasMore, p.NextCursor != "")
	}
}

// TestLibraryList_PastTheCapALongIDKeepsTheWalkGoing: past the offset cap only a cursor leads on, so a page there
// ending on a flow whose id is too long for one used to answer hasMore false, and the flows after it were out of reach.
// The web client's walk (the offset beside the cursor) now reaches every flow.
func TestLibraryList_PastTheCapALongIDKeepsTheWalkGoing(t *testing.T) {
	backend := testutil.NewFakeBackend()
	rt := newTestRouter(backend, true)
	const limit = 200
	end := maxListOffset + limit - 1 // the last flow of the first page past the cap
	ids := seedOrderedFlows(t, backend, "alice", end+31, map[int]bool{end: true})
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")
	var walked []string
	cursor := ""
	for page := 0; page <= len(ids)/limit+1; page++ {
		path := fmt.Sprintf("/api/library/?limit=%d&offset=%d", limit, page*limit)
		if cursor != "" {
			path += "&cursor=" + url.QueryEscape(cursor)
		}
		rr := doRequestWithAuth(t, rt, http.MethodGet, path, bearer, nil)
		checkStatus(t, rr, http.StatusOK)
		var p cursorPage
		decodeJSON(t, rr, &p)
		walked = append(walked, p.ids()...)
		if !p.HasMore {
			break
		}
		cursor = p.NextCursor
		if cursor == "" && (page+1)*limit > maxListOffset {
			t.Fatalf("page %d: more, but neither a cursor nor an offset reaches the next page", page)
		}
	}
	if !slices.Equal(walked, ids) {
		t.Errorf("the web walk read %d flows of %d; want every flow once, in order", len(walked), len(ids))
	}
}
