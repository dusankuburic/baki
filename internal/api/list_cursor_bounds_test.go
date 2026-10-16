package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"pad-analyzer/internal/auth"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// The bounds of the list cursors and offsets: what a page carries forward, what the server refuses, and where an
// offset-paged list ends.

// TestLibraryList_KeysetPagesCarryTheFirstPagesTotal: the first page counts the matching flows; the cursors of the
// pages after it carry that total, so a keyset page reads its own rows only and reports the first page's total, even
// when flows were added meanwhile. A page reached by offset counts again.
func TestLibraryList_KeysetPagesCarryTheFirstPagesTotal(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	for i := range 7 {
		seed(fmt.Sprintf("flow-%d", i), "alice")
	}
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")
	var first cursorPage
	decodeJSON(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/library/?limit=3", bearer, nil), &first)
	if first.Total != 7 || first.NextCursor == "" {
		t.Fatalf("first page = total %d, cursor %q; want 7 and a cursor", first.Total, first.NextCursor)
	}
	seed("flow-new-1", "alice")
	seed("flow-new-2", "alice")
	var next cursorPage
	decodeJSON(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/library/?limit=3&cursor="+url.QueryEscape(first.NextCursor), bearer, nil), &next)
	if next.Total != 7 || next.TotalCapped || len(next.Items) != 3 {
		t.Errorf("keyset page = total %d (capped %v), %d items; want the first page's 7, 3 items", next.Total, next.TotalCapped, len(next.Items))
	}
	var byOffset cursorPage
	decodeJSON(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/library/?limit=3&offset=3", bearer, nil), &byOffset)
	if byOffset.Total != 9 {
		t.Errorf("offset page total %d, want 9 (counted)", byOffset.Total)
	}
}

// TestLibraryList_OffsetStopsAtTheCap: an offset above maxListOffset is a 400 (it used to answer the page at the cap
// under any offset, so a client following hasMore by offset looped). A page at the cap of a sort without cursors says
// no more follow, since no offset reaches them; a sort with cursors keeps hasMore and gives the cursor.
func TestLibraryList_OffsetStopsAtTheCap(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	for i := range maxListOffset + 15 {
		seed(fmt.Sprintf("flow-%05d", i), "alice")
	}
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, fmt.Sprintf("/api/library/?offset=%d", maxListOffset+1), bearer, nil), http.StatusBadRequest)

	var p cursorPage
	decodeJSON(t, doRequestWithAuth(t, rt, http.MethodGet, fmt.Sprintf("/api/library/?sort=name_asc&limit=10&offset=%d", maxListOffset-5), bearer, nil), &p)
	if len(p.Items) != 10 || p.HasMore || p.NextCursor != "" {
		t.Errorf("name sort at the cap = %d items, more %v, cursor %q; want 10, no more (no offset reaches them), no cursor", len(p.Items), p.HasMore, p.NextCursor)
	}
	p = cursorPage{}
	decodeJSON(t, doRequestWithAuth(t, rt, http.MethodGet, fmt.Sprintf("/api/library/?sort=name_asc&limit=10&offset=%d", maxListOffset-10), bearer, nil), &p)
	if !p.HasMore {
		t.Errorf("name sort, the page ending at the cap: more %v; want more (offset %d is allowed)", p.HasMore, maxListOffset)
	}
	p = cursorPage{}
	decodeJSON(t, doRequestWithAuth(t, rt, http.MethodGet, fmt.Sprintf("/api/library/?limit=10&offset=%d", maxListOffset), bearer, nil), &p)
	if !p.HasMore || p.NextCursor == "" {
		t.Errorf("updated sort at the cap: more %v, cursor %q; want more and a cursor past the cap", p.HasMore, p.NextCursor)
	}
}

// TestListCursor_RefusesAForgedID: a cursor's id goes into the statement; NUL (which JSON's \u0000 decodes to and
// Postgres refuses in text, a 500), invalid UTF-8 and an id longer than any the server issues are refused as 400s.
func TestListCursor_RefusesAForgedID(t *testing.T) {
	at := time.Now().UTC()
	forged := func(id string) string {
		raw, err := json.Marshal(listCursor{Order: "updated_desc", Filter: "f", At: at, ID: id})
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	for name, id := range map[string]string{
		"NUL":       "a\x00b",
		"too long":  strings.Repeat("x", maxListCursorIDLen+1),
		"bad UTF-8": "a\xffb",
	} {
		raw := forged(id)
		if name == "bad UTF-8" {
			// json.Marshal would replace the byte; put it in the JSON as it comes.
			raw = base64.RawURLEncoding.EncodeToString([]byte(`{"o":"updated_desc","f":"f","t":"` + at.Format(time.RFC3339Nano) + `","i":"a` + "\xff" + `b"}`))
		}
		if c, err := decodeListCursor(raw, "updated_desc", "f"); err == nil {
			t.Errorf("%s: decoded %+v; want errInvalidCursor", name, c)
		}
	}
	if c, err := decodeListCursor(forged("flow-1"), "updated_desc", "f"); err != nil || c.ID != "flow-1" {
		t.Errorf("a well-formed cursor = %+v, %v", c, err)
	}
}

// TestListCursor_SkipStaysInBounds: a cursor's skip (the flows with ids too long for a cursor that the next page passes
// over) reads as an offset after the position, so it is bounded like one: a negative one or one above maxListOffset is
// refused, and the server never issues one; a page reached by cursor that gives none says no more follow, since its
// offset is not its position.
func TestListCursor_SkipStaysInBounds(t *testing.T) {
	at := time.Now().UTC()
	encode := func(skip int) string {
		raw, err := json.Marshal(listCursor{Order: "updated_desc", Filter: "f", At: at, ID: "flow-1", S: skip})
		if err != nil {
			t.Fatal(err)
		}
		return base64.RawURLEncoding.EncodeToString(raw)
	}
	for _, skip := range []int{-1, maxListOffset + 1} {
		if c, err := decodeListCursor(encode(skip), "updated_desc", "f"); err == nil {
			t.Errorf("skip %d: decoded %+v; want errInvalidCursor", skip, c)
		}
	}
	if c, err := decodeListCursor(encode(3), "updated_desc", "f"); err != nil || c.Skip != 3 || c.ID != "flow-1" {
		t.Errorf("skip 3 = %+v, %v", c, err)
	}
	docs := []*storageif.FlowDocument{{ID: longFlowID(1), UpdatedAt: at}}
	after := &storageif.PageCursor{At: at, ID: "flow-0", Skip: maxListOffset}
	if next := nextFlowCursor("updated_desc", "f", docs, after, 5, false); next != "" || offsetPageHasMore(true, next, true, 0, 1) {
		t.Errorf("past the most a cursor passes over: cursor %q; want none, and no more", next)
	}
	after.Skip = 2
	next := nextFlowCursor("updated_desc", "f", docs, after, 5, false)
	if c, err := decodeListCursor(next, "updated_desc", "f"); err != nil || c.ID != "flow-0" || c.Skip != 3 || !c.Counted || c.Total != 5 {
		t.Errorf("a page of long ids continues from its own cursor: %+v, %v; want flow-0, skip 3, total 5", c, err)
	}
}

// TestLibraryList_NoCursorPastItsBound: a flow id long enough (a legacy import keeps the file name) to make the
// encoded cursor longer than the server accepts gives no cursor: the page says more follow and the next is reached by
// offset, instead of a cursor the server then refuses.
func TestLibraryList_NoCursorPastItsBound(t *testing.T) {
	rt, seed := newLibraryTestRouter(t)
	// 307 bytes, each of <, > and & six after JSON escaping; between the others in id order and in save order.
	long := "flow-b-" + strings.Repeat("<a&b>", 60)
	seed("flow-a", "alice")
	seed(long, "alice")
	seed("flow-c", "alice")
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")
	var all cursorPage
	decodeJSON(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/library/?sort=updated_asc&limit=10", bearer, nil), &all)
	ids := all.ids()
	if len(ids) != 3 {
		t.Fatalf("the library = %v", ids)
	}
	// One page per flow: whichever page ends on the long id must not carry a cursor the server refuses.
	for i := range 2 {
		var p cursorPage
		rr := doRequestWithAuth(t, rt, http.MethodGet, fmt.Sprintf("/api/library/?sort=updated_asc&limit=1&offset=%d", i), bearer, nil)
		decodeJSON(t, rr, &p)
		if !p.HasMore {
			t.Fatalf("page %d: no more; want more", i)
		}
		if p.NextCursor == "" {
			if ids[i] != long {
				t.Errorf("page %d ends on %s and gives no cursor", i, ids[i])
			}
			continue
		}
		if len(p.NextCursor) > maxListCursorLen {
			t.Errorf("page %d: a %d-byte cursor, over the %d the server accepts", i, len(p.NextCursor), maxListCursorLen)
		}
		next := doRequestWithAuth(t, rt, http.MethodGet, "/api/library/?sort=updated_asc&limit=1&cursor="+url.QueryEscape(p.NextCursor), bearer, nil)
		checkStatus(t, next, http.StatusOK)
	}
}

// TestAdminCursors_AreNotInterchangeable: the user list's cursor is refused by the audit log and the other way round,
// even with no filters on either (their filter digests are equal then).
func TestAdminCursors_AreNotInterchangeable(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "admin-1", "admin@example.com", auth.RoleAdmin)
	for i := range 3 {
		seedUserWithRole(t, rt, fmt.Sprintf("user-%02d", i), fmt.Sprintf("user-%02d@example.com", i), auth.RoleMember)
	}
	for i := range 3 {
		e := &storageif.AuditEvent{ID: fmt.Sprintf("ev-%02d", i), UserID: "user-00", Action: "flow.save", ResourceType: "flow",
			ResourceID: "f", CreatedAt: time.Now().UTC().Add(-time.Duration(i) * time.Minute)}
		if err := rt.security.Backend.SaveAuditEvent(context.Background(), e); err != nil {
			t.Fatal(err)
		}
	}
	bearer := jwtBearer(t, rt, "admin-1", "admin@example.com")
	var users cursorPage
	decodeJSON(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/admin/users/list?limit=1", bearer, nil), &users)
	audit := doRequestWithAuth(t, rt, http.MethodGet, "/api/admin/audit?limit=1", bearer, nil)
	auditCursor := audit.Header().Get(nextCursorHeader)
	if users.NextCursor == "" || auditCursor == "" {
		t.Fatalf("cursors: users %q, audit %q", users.NextCursor, auditCursor)
	}
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/admin/audit?limit=1&cursor="+url.QueryEscape(users.NextCursor), bearer, nil), http.StatusBadRequest)
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, "/api/admin/users/list?limit=1&cursor="+url.QueryEscape(auditCursor), bearer, nil), http.StatusBadRequest)
	// Above the cap the offset is refused, as on the library.
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, fmt.Sprintf("/api/admin/audit?offset=%d", maxListOffset+1), bearer, nil), http.StatusBadRequest)
	checkStatus(t, doRequestWithAuth(t, rt, http.MethodGet, fmt.Sprintf("/api/admin/users/list?offset=%d", maxListOffset+1), bearer, nil), http.StatusBadRequest)
}
