package api

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"pad-analyzer/internal/api/render"
	storageif "pad-analyzer/internal/storage/interfaces"
)

// The keyset cursors of the offset-paged lists (the library, the admin user list and audit log): a page's next
// cursor names its last item's position, and the next page starts after it without re-reading the rows before it.
// A request without a cursor pages by offset as before, the offset at most maxListOffset.

// maxListOffset caps an offset-paged list's offset: OFFSET reads and drops every row it skips, so a deep page cost
// what all the pages before it did. A client that pages deeper takes the cursor; a larger offset is a 400.
const maxListOffset = 10_000

// nextCursorHeader carries the audit log's next cursor: that list answers a bare array, which has no field for it.
const nextCursorHeader = "X-Next-Cursor"

// The keyset orders of the admin lists, one name each: a cursor is honoured only by the list that issued it (with no
// filters their filter digests are equal).
const (
	adminUserListOrder = "users_created_desc"
	adminAuditOrder    = "audit_created_desc"
)

// listCursor is a cursor's content, base64url-encoded JSON: the order it is a position in, a digest of the filters
// it was issued under, the last item's time and id, and, for a list that counts, the total its first page counted (N,
// C: capped). S counts the items after that one the next page passes over (the library's: their ids do not fit a
// cursor). Opaque to clients (docs/architecture/api-conventions.md).
type listCursor struct {
	Order  string    `json:"o"`
	Filter string    `json:"f"`
	At     time.Time `json:"t"`
	ID     string    `json:"i"`
	N      *int      `json:"n,omitempty"`
	C      bool      `json:"c,omitempty"`
	S      int       `json:"s,omitempty"`
}

// maxListCursorLen bounds an incoming cursor; one the server issued is well under it.
const maxListCursorLen = 512

// maxListCursorIDLen bounds a cursor's id, which the next page's statement compares with.
const maxListCursorIDLen = 256

// errInvalidCursor answers a cursor the list did not issue: malformed, for another order, or for other filters.
var errInvalidCursor = errors.New("invalid cursor: pass the previous page's nextCursor, with the same filters and sort")

// listFilterDigest identifies a list's filter values, so a cursor is honoured only with the filters it was issued for.
func listFilterDigest(filters ...string) string {
	h := sha256.New()
	for _, f := range filters {
		h.Write([]byte(f))
		h.Write([]byte{0})
	}
	return hex.EncodeToString(h.Sum(nil)[:8])
}

// encodeListCursor is the cursor after the item at (at, id) of a list in order under filters (listFilterDigest); ""
// when it would be one decodeListCursor refuses (an id too long or not text, a cursor over maxListCursorLen): the
// page then gives no cursor and the client pages on by offset.
func encodeListCursor(order, filters string, at time.Time, id string) string {
	return encodeCursor(listCursor{Order: order, Filter: filters, At: at, ID: id})
}

// nextFlowCursor is the cursor after a library page of docs, reached by after (nil: by offset): it carries the total
// the page reports (counted on the first page, capped: it stopped at the cap), so the pages it leads to need not count
// again. It names the page's last flow whose id fits a cursor, the next page passing over the flows after that one; a
// page with none such continues from after, passing over the whole page too. "" only for an offset page with none such
// (the client pages on by offset, which is exact there) or past maxListOffset passed-over flows.
func nextFlowCursor(order, filters string, docs []*storageif.FlowDocument, after *storageif.PageCursor, total int, capped bool) string {
	for i := len(docs) - 1; i >= 0; i-- {
		if c := encodeCursor(listCursor{Order: order, Filter: filters, At: docs[i].UpdatedAt, ID: docs[i].ID, N: &total, C: capped, S: len(docs) - 1 - i}); c != "" {
			return c
		}
	}
	if after == nil {
		return ""
	}
	return encodeCursor(listCursor{Order: order, Filter: filters, At: after.At, ID: after.ID, N: &total, C: capped, S: after.Skip + len(docs)})
}

func encodeCursor(c listCursor) string {
	if !validCursorID(c.ID) || c.S < 0 || c.S > maxListOffset {
		return ""
	}
	raw, err := json.Marshal(c)
	if err != nil {
		return "" // a time, three strings and a number always marshal
	}
	if enc := base64.RawURLEncoding.EncodeToString(raw); len(enc) <= maxListCursorLen {
		return enc
	}
	return ""
}

// validCursorID reports whether id may stand in a cursor: non-empty text Postgres takes (no NUL), at most
// maxListCursorIDLen bytes.
func validCursorID(id string) bool {
	return id != "" && len(id) <= maxListCursorIDLen && utf8.ValidString(id) && !strings.ContainsRune(id, 0)
}

// decodeListCursor reads a ?cursor= value: nil without one, errInvalidCursor for one this list did not issue under
// order and filters. A list order without keyset paging ("") accepts none.
func decodeListCursor(raw, order, filters string) (*storageif.PageCursor, error) {
	if raw == "" {
		return nil, nil
	}
	if order == "" || len(raw) > maxListCursorLen {
		return nil, errInvalidCursor
	}
	data, err := base64.RawURLEncoding.DecodeString(raw)
	if err != nil || !utf8.Valid(data) {
		return nil, errInvalidCursor
	}
	var c listCursor
	if err := json.Unmarshal(data, &c); err != nil || c.Order != order || c.Filter != filters || !validCursorID(c.ID) || c.At.IsZero() ||
		c.S < 0 || c.S > maxListOffset {
		return nil, errInvalidCursor
	}
	pc := &storageif.PageCursor{At: c.At, ID: c.ID, Skip: c.S}
	if c.N != nil && *c.N >= 0 {
		pc.Counted, pc.Total, pc.TotalCapped = true, *c.N, c.C
	}
	return pc, nil
}

// listOffset parses an optional ?offset= (default 0, a negative one 0) of a list paged up to maxListOffset. Without a
// cursor a larger one is answered with a 400 here: it used to read as the cap, so a client paging by offset got the
// same page again. With one the offset is ignored (a client sends both, for a server without cursors), and reads as
// at most the cap.
func listOffset(w http.ResponseWriter, raw string, cursor bool) (offset int, ok bool) {
	offset, ok = clampListOffset(w, raw)
	if ok && !cursor && offset > maxListOffset {
		render.Error(w, fmt.Errorf("invalid %q parameter: at most %d; page on with the cursor", "offset", maxListOffset), http.StatusBadRequest)
		return 0, false
	}
	return min(offset, maxListOffset), ok
}

// offsetPageHasMore is a page's hasMore as a client can act on it: a page with no next cursor has nothing more it can
// reach when it was itself reached by cursor (its offset is not its position) or its next offset is above
// maxListOffset.
func offsetPageHasMore(hasMore bool, nextCursor string, byCursor bool, offset, limit int) bool {
	return hasMore && (nextCursor != "" || (!byCursor && offset+limit <= maxListOffset))
}
