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
		// Exempted routes are mutating POSTs (see routes_chi.go) plus the
		// read-only GET dashboard aggregate; verify the exemption key matches
		// a registered route of one of those methods.
		if !registered["POST "+path] && !registered["GET "+path] {
			t.Errorf("rlsHeavyExactPaths entry %q is not a registered GET/POST route (registered set drifted?)", path)
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
		{"/api/dashboard/analytics", true},
		// NOT exempt: exact-match tree siblings.
		{"/api/analysis/analyze-raw/extra", false},
		{"/api/analysis/triage/set", false},
		{"/api/analysis/comments/add", false},
		{"/api/analysis/policy/save", false},
		{"/api/analysis/baseline/set", false},
		{"/api/flow/load-path", false},
		{"/api/library", false},
		// Provider routes that dial outbound (live listings, the connection
		// test, the GitHub device flow) must not hold the RLS transaction's
		// pooled connection across the upstream call.
		{"/api/providers/list", true},
		{"/api/providers/test", true},
		{"/api/providers/github/start", true},
		{"/api/providers/github/poll", true},
		{"/api/providers/github/user", true},
		{"/api/providers/copilot/start", true},
		{"/api/providers/copilot/poll", true},
		{"/api/providers/copilot/user", true},
		// But their purely local siblings stay transactional: revoke is a
		// keystore delete, and the azure-foundry config routes validate
		// offline (no DNS, no network I/O).
		{"/api/providers/github/revoke", false},
		{"/api/providers/copilot/revoke", false},
		{"/api/providers/azure-foundry/config", false},
		// The Azure AI Foundry registry: its test and discovery routes dial
		// Azure (the caller's own and an organisation's); its CRUD routes
		// keep their transaction.
		{"/api/providers/azure-foundry/test", true},
		{"/api/providers/azure-foundry/connections/abcdefghij/discover", true},
		{"/api/orgs/org-1/providers/azure-foundry/test", true},
		{"/api/orgs/org-1/providers/azure-foundry/connections/default/discover", true},
		{"/api/providers/azure-foundry/connections", false},
		{"/api/providers/azure-foundry/connections/abcdefghij", false},
		{"/api/providers/azure-foundry/models/abcdefghij", false},
		{"/api/orgs/org-1/providers/azure-foundry/models", false},
		{"/api/orgs/org-1/providers/azure-foundry/connections/abcdefghij", false},
		// And a path under the provider tree that is no route at all.
		{"/api/providers/other", false},
		// Blob-content library routes: exempt (blob transfer must not pin the
		// RLS transaction's pooled connection).
		{"/api/library/abc/content", true},
		{"/api/library/abc/versions/3", true},
		{"/api/library/abc/versions/3/diff", true},
		// But the mutating version routes keep their RLS transaction.
		{"/api/library/abc/versions", false},
		{"/api/library/abc/versions/3/restore", false},
		// And non-blob library routes stay transactional.
		{"/api/library/abc", false},
		{"/api/library/abc/tags", false},
		// The knowledge re-index commits page by page (R-7): outside the
		// request transaction. Its siblings keep theirs — an upload is one
		// atomic document write.
		{"/api/orgs/org-1/knowledge/reindex", true},
		{"/api/orgs/org-1/knowledge/upload", false},
		{"/api/orgs/org-1/knowledge", false},
		{"/api/orgs/org-1/knowledge/doc-1", false},
		{"/api/orgs//knowledge/reindex", false},
		{"/api/orgs/org-1/x/knowledge/reindex", false},
	}
	for _, c := range cases {
		if got := rlsExemptPath(c.path); got != c.exempt {
			t.Errorf("rlsExemptPath(%q) = %v, want %v", c.path, got, c.exempt)
		}
	}
}
