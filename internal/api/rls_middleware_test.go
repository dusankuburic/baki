package api

import (
	"net/http"
	"testing"

	"github.com/go-chi/chi/v5"

	"pad-analyzer/internal/storage/filesystem"
)

// TestRLSHeavyExemptPathsAreRegisteredRoutes guards the RLS heavy-path
// exemption list against drift: every entry must match a real registered
// route (method + path). A stale entry after a route rename would silently do
// nothing — the renamed route would keep its per-request RLS transaction —
// while giving the false impression it is exempt.
func TestRLSHeavyExemptPathsAreRegisteredRoutes(t *testing.T) {
	fs, err := filesystem.NewLocalStorageBackend(t.TempDir())
	if err != nil {
		t.Fatalf("backend: %v", err)
	}
	rt := newTestRouter(fs, false)

	// Collect "METHOD /path" for every registered route.
	registered := map[string]bool{}
	walkErr := chi.Walk(rt.mux, func(method, route string, _ http.Handler, _ ...func(http.Handler) http.Handler) error {
		registered[method+" "+route] = true
		return nil
	})
	if walkErr != nil {
		t.Fatalf("walk routes: %v", walkErr)
	}

	for path := range rlsHeavyExactPaths {
		// All exempted routes are POSTs (see routes_chi.go); verify both that
		// the POST exists and that the exemption key matches its exact path.
		if !registered["POST "+path] {
			t.Errorf("rlsHeavyExactPaths entry %q is not a registered POST route (registered set drifted?)", path)
		}
	}
}

// TestRLSExemptPath_matching pins the prefix-vs-exact matching semantics:
// streaming prefixes exempt subpaths, heavy paths must match exactly so
// sibling routes under the same tree keep their RLS transaction.
func TestRLSExemptPath_matching(t *testing.T) {
	cases := []struct {
		path   string
		exempt bool
	}{
		{"/api/events", true},
		{"/api/chat/stream", true},
		{"/api/analysis/analyze", true},
		{"/api/analysis/analyze-raw", true},
		// NOT exempt: exact-match tree siblings.
		{"/api/analysis/analyze-raw/extra", false},
		{"/api/analysis/triage/set", false},
		{"/api/analysis/comments/add", false},
		{"/api/analysis/policy/save", false},
		{"/api/analysis/baseline/set", false},
		{"/api/flow/load-path", false},
		{"/api/library", false},
	}
	for _, c := range cases {
		if got := rlsExemptPath(c.path); got != c.exempt {
			t.Errorf("rlsExemptPath(%q) = %v, want %v", c.path, got, c.exempt)
		}
	}
}
