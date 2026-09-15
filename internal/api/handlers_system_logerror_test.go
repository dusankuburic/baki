package api

import (
	"net/http"
	"strings"
	"testing"
)

// TestLogError_BodyIsBounded: POST /api/system/log-error used to accept the
// router's 10 MiB global cap and write all of it to the ERROR log. Anything
// past 64 KiB is refused with 413; a normal report still lands.
func TestLogError_BodyIsBounded(t *testing.T) {
	rt := newJWTTestRouter(t)
	seedUserWithRole(t, rt, "u-1", "u@example.com", "member")
	bearer := jwtBearer(t, rt, "u-1", "u@example.com")

	rr := doRequestWithAuth(t, rt, http.MethodPost, "/api/system/log-error", bearer,
		map[string]any{"message": "boom", "stack": strings.Repeat("x", 100<<10)})
	if rr.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("100 KiB report: want 413, got %d (body: %s)", rr.Code, rr.Body.String())
	}
	rr = doRequestWithAuth(t, rt, http.MethodPost, "/api/system/log-error", bearer,
		map[string]any{"message": "boom", "stack": "at x (y.js:1)"})
	if rr.Code != http.StatusOK {
		t.Fatalf("small report: want 200, got %d (body: %s)", rr.Code, rr.Body.String())
	}
}
