package api

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"pad-analyzer/internal/auth"
)

func TestDisplayFromEmail(t *testing.T) {
	cases := []struct {
		email string
		want  string
	}{
		{"ada.lovelace@example.com", "ada.lovelace"},
		{"bob@x.com", "bob"},
		{"no-email", "no-email"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := displayFromEmail(tc.email); got != tc.want {
			t.Errorf("displayFromEmail(%q) = %q, want %q", tc.email, got, tc.want)
		}
	}
}

func TestGreeting_LocalMode(t *testing.T) {
	sec := &SecurityConfig{JWTEnabled: false, LocalName: "TestUser"}
	h := &DashboardHandler{security: sec}
	g := h.greeting(&http.Request{})
	if g.UserDisplayName != "TestUser" {
		t.Errorf("expected 'TestUser', got %q", g.UserDisplayName)
	}
}

// TestHandleHome_ConcurrentCacheHitRaceFree is the handler-level regression
// for the Greeting variant of the dashboard cache race: on a TTL-cache hit
// BuildHome returns a SHARED pointer, and handleHome previously wrote
// data.Greeting on that shared struct before marshalling — racing every other
// concurrent caller's marshal of the same struct. The handler now copies.
// Under `go test -race`, any write-after-handoff recurrence is flagged.
func TestHandleHome_ConcurrentCacheHitRaceFree(t *testing.T) {
	rt := newJWTTestRouter(t)
	tok := jwtBearer(t, rt, "u1", "u1@example.com")

	// Prime the dashboard cache for user "u1" so subsequent calls take the
	// shared-pointer cache-hit path (cloud mode with JWT enabled).
	req1 := httptest.NewRequest(http.MethodGet, "/api/dashboard/home", nil)
	req1 = req1.WithContext(auth.WithClaims(req1.Context(), &auth.Claims{UserID: "u1", Email: "u1@example.com"}))
	req1.Header.Set("Authorization", tok)
	rec1 := httptest.NewRecorder()
	rt.ServeHTTP(rec1, req1)
	if rec1.Code != http.StatusOK {
		t.Fatalf("prime request: status %d", rec1.Code)
	}

	var wg sync.WaitGroup
	for range 50 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			req := httptest.NewRequest(http.MethodGet, "/api/dashboard/home", nil)
			req = req.WithContext(auth.WithClaims(req.Context(), &auth.Claims{UserID: "u1", Email: "u1@example.com"}))
			req.Header.Set("Authorization", tok)
			rec := httptest.NewRecorder()
			rt.ServeHTTP(rec, req) // marshals the shared struct concurrently
			if rec.Code != http.StatusOK {
				t.Errorf("concurrent request: status %d", rec.Code)
			}
		}()
	}
	wg.Wait()
}
