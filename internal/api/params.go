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
