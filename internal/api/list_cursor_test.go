package api

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/service"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// The keyset cursors of the library, the admin user list and the audit log (v49), beside the offsets the older
// clients send.

type cursorPage struct {
	Items       []map[string]any `json:"items"`
	Total       int              `json:"total"`
	Offset      int              `json:"offset"`
	Limit       int              `json:"limit"`
	HasMore     bool             `json:"hasMore"`
	NextCursor  string           `json:"nextCursor"`
	TotalCapped bool             `json:"totalCapped"`
}

func (p cursorPage) ids() []string {
	out := []string{}
	for _, it := range p.Items {
		out = append(out, fmt.Sprint(it["id"]))
	}
	return out
}

// walkPages follows nextCursor from first (a path with a query) until the last page and returns the pages.
func walkPages(t *testing.T, rt *Router, bearer, first string) []cursorPage {
	t.Helper()
	var pages []cursorPage
	path := first
	for range 20 {
		rr := doRequestWithAuth(t, rt, http.MethodGet, path, bearer, nil)
		checkStatus(t, rr, http.StatusOK)
		var p cursorPage
		decodeJSON(t, rr, &p)
		pages = append(pages, p)
		if p.NextCursor == "" {
			if p.HasMore {
				t.Errorf("%s: hasMore without a nextCursor", path)
			}
			return pages
		}
		if !p.HasMore {
			t.Errorf("%s: a nextCursor on the last page", path)
		}
		path = first + "&cursor=" + url.QueryEscape(p.NextCursor)
	}
	t.Fatalf("%s: the walk does not end", first)
	return nil
}

// TestLibraryList_PagesByCursor: following nextCursor yields the list's order exactly, with hasMore on every page
// but the last, the total on each; the offset pages agree; a sort without keyset paging gives no cursor.
func TestLibraryList_PagesByCursor(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	for i := range 7 {
		seed(fmt.Sprintf("flow-%d", i), "alice")
	}
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")

	var all cursorPage
	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/library/?limit=200", bearer, nil)
	decodeJSON(t, rr, &all)
	if len(all.Items) != 7 || all.HasMore || all.NextCursor != "" || all.Total != 7 || all.TotalCapped {
		t.Fatalf("one page of everything = %+v", all)
	}

	for _, sort := range []string{"", "updated_asc"} {
		var whole cursorPage
		decodeJSON(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/library/?limit=200&sort="+sort, bearer, nil), &whole)
		var walked []string
		for i, p := range walkPages(t, rt, bearer, "/api/library/?limit=3&sort="+sort) {
			walked = append(walked, p.ids()...)
			if p.Total != 7 {
				t.Errorf("sort %q, page %d: total %d, want 7", sort, i, p.Total)
			}
		}
		if !slices.Equal(walked, whole.ids()) {
			t.Errorf("sort %q: the cursor walk %v, the whole list %v", sort, walked, whole.ids())
		}
	}

	// The offset pages, as the older clients ask for them, say whether more follow too.
	p := cursorPage{}
	decodeJSON(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/library/?limit=3&offset=3", bearer, nil), &p)
	if !slices.Equal(p.ids(), all.ids()[3:6]) || !p.HasMore || p.Total != 7 {
		t.Errorf("offset 3 = %v more=%v total=%d; want %v, more, 7", p.ids(), p.HasMore, p.Total, all.ids()[3:6])
	}
	p = cursorPage{}
	decodeJSON(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/library/?limit=3&offset=6", bearer, nil), &p)
	if !slices.Equal(p.ids(), all.ids()[6:]) || p.HasMore || p.NextCursor != "" {
		t.Errorf("offset 6 = %v more=%v; want the last flow, no more", p.ids(), p.HasMore)
	}
	// An offset above the cap is refused (TestLibraryList_OffsetStopsAtTheCap); at the cap it pages past the end.
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/library/?limit=3&offset=999999", bearer, nil), http.StatusBadRequest)
	p = cursorPage{}
	decodeJSON(t, doRequestWithAuth(t, rt, http.MethodGet, fmt.Sprintf("/api/library/?limit=3&offset=%d", maxListOffset), bearer, nil), &p)
	if p.Offset != maxListOffset || len(p.Items) != 0 || p.Total != 7 || p.HasMore {
		t.Errorf("offset at the cap = offset %d, %d items, total %d, more %v; want offset %d, none, 7, no more", p.Offset, len(p.Items), p.Total, p.HasMore, maxListOffset)
	}
	// Name sorts page by offset: no cursor.
	p = cursorPage{}
	decodeJSON(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/library/?limit=3&sort=name_asc", bearer, nil), &p)
	if !p.HasMore || p.NextCursor != "" {
		t.Errorf("name sort: more=%v cursor=%q; want more and no cursor", p.HasMore, p.NextCursor)
	}
}

// TestLibraryList_RefusesACursorItDidNotIssue: a cursor is honoured only with the sort and filters it was issued
// for; anything else is a 400, never a page from another list's position.
func TestLibraryList_RefusesACursorItDidNotIssue(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	for i := range 4 {
		seed(fmt.Sprintf("flow-%d", i), "alice")
	}
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")
	var p cursorPage
	decodeJSON(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/library/?limit=2", bearer, nil), &p)
	if p.NextCursor == "" {
		t.Fatal("no cursor on the first page")
	}
	c := url.QueryEscape(p.NextCursor)
	for _, path := range []string{
		"/api/library/?limit=2&sort=updated_asc&cursor=" + c,
		"/api/library/?limit=2&sort=name_asc&cursor=" + c,
		"/api/library/?limit=2&q=flow&cursor=" + c,
		"/api/library/?limit=2&scope=mine&cursor=" + c,
		"/api/library/?limit=2&tags=prod&cursor=" + c,
		"/api/library/?limit=2&cursor=not-a-cursor",
		"/api/library/?limit=2&cursor=" + url.QueryEscape(encodeListCursor("updated_desc", "other", time.Now(), "x")),
	} {
		rr := doRequestWithAuth(t, rt, http.MethodGet, path, bearer, nil)
		checkStatus(t, rr, http.StatusBadRequest)
	}
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/library/?limit=2&sort=updated_desc&cursor="+c, bearer, nil), http.StatusOK)
}

// TestLibraryList_TotalIsCapped: past service.LibraryTotalCap flows the total is the cap with totalCapped, and the
// pages still say whether more follow.
func TestLibraryList_TotalIsCapped(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	for i := range service.LibraryTotalCap + 1 {
		seed(fmt.Sprintf("flow-%05d", i), "alice")
	}
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")
	var p cursorPage
	decodeJSON(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/library/?limit=10", bearer, nil), &p)
	if p.Total != service.LibraryTotalCap || !p.TotalCapped || !p.HasMore || len(p.Items) != 10 {
		t.Errorf("page of %d flows: total %d capped %v more %v items %d; want %d, capped, more, 10",
			service.LibraryTotalCap+1, p.Total, p.TotalCapped, p.HasMore, len(p.Items), service.LibraryTotalCap)
	}
}

// TestAdminUserList_PagesByCursor: the admin user list's nextCursor walks the filtered list in its order, the total
// counting every match on each page; a cursor for other filters is a 400.
func TestAdminUserList_PagesByCursor(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	for i := range 7 {
		seedUserWithRole(t, rt, fmt.Sprintf("user-%02d", i), fmt.Sprintf("user-%02d@example.com", i), auth.RoleMember)
	}
	bearer := jwtBearer(t, rt, "admin-1", "admin@example.com")

	var whole cursorPage
	decodeJSON(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/admin/users/list?limit=100&role=member", bearer, nil), &whole)
	var walked []string
	pages := walkPages(t, rt, bearer, "/api/admin/users/list?limit=3&role=member")
	for _, p := range pages {
		walked = append(walked, p.ids()...)
		if p.Total != 7 {
			t.Errorf("page total %d, want 7", p.Total)
		}
	}
	if len(pages) != 3 || !slices.Equal(walked, whole.ids()) {
		t.Errorf("the cursor walk %v in %d pages, the whole list %v", walked, len(pages), whole.ids())
	}
	c := url.QueryEscape(pages[0].NextCursor)
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/admin/users/list?limit=3&role=admin&cursor="+c, bearer, nil), http.StatusBadRequest)
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/admin/users/list?limit=3&cursor=zzz", bearer, nil), http.StatusBadRequest)
	// An offset above the cap is refused; at the cap it pages past the end.
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/admin/users/list?offset=50000", bearer, nil), http.StatusBadRequest)
	var p cursorPage
	decodeJSON(t, doRequestWithAuth(t, rt, http.MethodGet, fmt.Sprintf("/api/admin/users/list?offset=%d", maxListOffset), bearer, nil), &p)
	if p.Offset != maxListOffset || len(p.Items) != 0 || p.HasMore {
		t.Errorf("offset at the cap = offset %d, %d items, more %v", p.Offset, len(p.Items), p.HasMore)
	}
}

// TestAdminAuditList_PagesByCursorHeader: the audit log stays a bare array; a page with events after it carries the
// next cursor in X-Next-Cursor, which walks the log in its order. A full page with none after it carries none (it used
// to, and the page it led to was empty), so the header alone tells whether a next page exists.
func TestAdminAuditList_PagesByCursorHeader(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	bearer := jwtBearer(t, rt, "admin-1", "admin@example.com")
	base := time.Now().UTC().Add(-time.Hour)
	var want []string
	for i := range 7 {
		e := &storageif.AuditEvent{ID: fmt.Sprintf("ev-%02d", i), UserID: "user-x", Action: "flow.save", ResourceType: "flow",
			ResourceID: "f", CreatedAt: base.Add(time.Duration(i/2) * time.Minute)} // pairs share a time: the id breaks the tie
		if err := rt.security.Backend.SaveAuditEvent(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	for _, i := range []int{6, 4, 5, 2, 3, 0, 1} {
		want = append(want, fmt.Sprintf("ev-%02d", i))
	}

	var walked []string
	path := "/api/admin/audit?userId=user-x&limit=3"
	for range 10 {
		rr := doRequestWithAuth(t, rt, http.MethodGet, path, bearer, nil)
		checkStatus(t, rr, http.StatusOK)
		var evs []map[string]any
		decodeJSON(t, rr, &evs)
		for _, e := range evs {
			walked = append(walked, fmt.Sprint(e["id"]))
		}
		next := rr.Header().Get(nextCursorHeader)
		if (next != "") != (len(walked) < len(want)) {
			t.Errorf("%s: %d events, %d of %d walked, cursor %q; want one while events follow", path, len(evs), len(walked), len(want), next)
		}
		if next == "" {
			break
		}
		path = "/api/admin/audit?userId=user-x&limit=3&offset=1&cursor=" + url.QueryEscape(next)
	}
	if !slices.Equal(walked, want) {
		t.Errorf("the cursor walk %v, want %v", walked, want)
	}
	for _, limit := range []int{7, 8} {
		rr := doRequestWithAuth(t, rt, http.MethodGet, fmt.Sprintf("/api/admin/audit?userId=user-x&limit=%d", limit), bearer, nil)
		checkStatus(t, rr, http.StatusOK)
		if next := rr.Header().Get(nextCursorHeader); next != "" {
			t.Errorf("limit %d, the whole log of 7: cursor %q; want none", limit, next)
		}
	}
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/admin/audit?userId=other&cursor="+url.QueryEscape(encodeListCursor(adminAuditOrder, listFilterDigest("user-x", ""), base, "ev-01")), bearer, nil), http.StatusBadRequest)
}

// TestLibraryList_CursorBoundToCaller: a library cursor issued for one user cannot be replayed by another;
// attempting to do so is rejected with 400 Bad Request (invalid cursor), avoiding count/total information leak.
func TestLibraryList_CursorBoundToCaller(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	seedUserWithRole(t, rt, "bob", "bob@example.com", auth.RoleMember)
	for i := range 5 {
		seed(fmt.Sprintf("alice-flow-%d", i), "alice")
	}
	seed("bob-flow-1", "bob")

	aliceBearer := jwtBearer(t, rt, "alice", "alice@example.com")
	bobBearer := jwtBearer(t, rt, "bob", "bob@example.com")

	// Alice fetches page 1 and gets a cursor
	var p cursorPage
	rr := doRequestWithAuth(t, rt, http.MethodGet, "/api/library/?limit=2", aliceBearer, nil)
	checkStatus(t, rr, http.StatusOK)
	decodeJSON(t, rr, &p)
	if p.NextCursor == "" {
		t.Fatal("expected nextCursor on page 1")
	}

	// Alice uses her own cursor -> 200 OK
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/library/?limit=2&cursor="+url.QueryEscape(p.NextCursor), aliceBearer, nil), http.StatusOK)

	// Bob attempts to use Alice's cursor -> 400 Bad Request
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/library/?limit=2&cursor="+url.QueryEscape(p.NextCursor), bobBearer, nil), http.StatusBadRequest)
}
