package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"pad-analyzer/internal/api/render"
)

// POST /api/flow/search-library with a term without a run of three letters or digits ("ab", "---") is a 400
// QUERY_TOO_SHORT whose message a client
// that does not know the code (one released before it) can show as it is; a blank term still answers no results.
func TestSearchLibrary_ShortTermIs400(t *testing.T) {
	rt, _ := newLibraryTestRouter(t)
	bearer := jwtBearer(t, rt, "alice", "alice@example.com")
	for _, text := range []string{"a", "ab", " ab ", "日本", "---", "a b"} {
		rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/flow/search-library", bearer,
			map[string]any{"query": map[string]any{"text": text}})
		checkStatus(t, rr, http.StatusBadRequest)
		var env render.ErrorResponse
		if err := json.Unmarshal(rr.Body.Bytes(), &env); err != nil {
			t.Fatalf("%q: decode %s: %v", text, rr.Body.String(), err)
		}
		if env.Code != "QUERY_TOO_SHORT" || env.Message != "a library search needs at least 3 letters or digits in a row" {
			t.Errorf("%q: envelope %+v, want QUERY_TOO_SHORT with a readable message", text, env)
		}
		if got, _ := env.Details["minLength"].(float64); got != 3 {
			t.Errorf("%q: details %v, want minLength 3", text, env.Details)
		}
	}
	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/flow/search-library", bearer,
		map[string]any{"query": map[string]any{"text": ""}})
	checkStatus(t, rr, http.StatusOK)
}
