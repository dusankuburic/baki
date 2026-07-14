package api

import (
	"fmt"
	"net/http"
	"strconv"

	"pad-analyzer/internal/api/render"
)

// parseIntParam parses an optional integer request parameter (a query value or
// URL path param). An empty string yields def. A non-empty value that fails to
// parse is reported to the client as 400 Bad Request and ok is false, so the
// caller should simply return. This avoids silently coercing malformed input
// (e.g. "abc") to 0.
func parseIntParam(w http.ResponseWriter, raw, name string, def int) (value int, ok bool) {
	if raw == "" {
		return def, true
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		render.Error(w, fmt.Errorf("invalid %q parameter: must be an integer", name), http.StatusBadRequest)
		return 0, false
	}
	return v, true
}

// maxListItems bounds unbounded list responses so a large table can't force a
// huge payload into memory and over the wire. Callers may request a smaller
// page via ?limit=; values <= 0 or above the cap collapse to the cap.
const maxListItems = 200

// clampListLimit parses an optional ?limit= query value (defaulting to
// maxListItems) and clamps it into [1, maxListItems]. On a malformed value it
// writes a 400 and returns ok=false.
func clampListLimit(w http.ResponseWriter, raw string) (limit int, ok bool) {
	limit, ok = parseIntParam(w, raw, "limit", maxListItems)
	if !ok {
		return 0, false
	}
	if limit <= 0 || limit > maxListItems {
		limit = maxListItems
	}
	return limit, true
}

// clampListOffset parses an optional ?offset= query value (defaulting to 0) and
// clamps it to >= 0. A negative offset would otherwise flow into SQL as
// "OFFSET -1" and surface as a confusing 500 instead of a benign clamp.
func clampListOffset(w http.ResponseWriter, raw string) (offset int, ok bool) {
	offset, ok = parseIntParam(w, raw, "offset", 0)
	if !ok {
		return 0, false
	}
	if offset < 0 {
		offset = 0
	}
	return offset, true
}
