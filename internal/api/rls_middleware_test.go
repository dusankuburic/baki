package api

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

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
		// In-flow search builds and walks the whole document's index with
		// nothing to write; its library-wide sibling was already exempt.
		{"/api/flow/search", true},
		{"/api/flow/search-library", true},
		// Document exports render for seconds (PDF rasteriser, HTML template,
		// Markdown) and base64-encode the result; the desktop-only siblings
		// keep their transaction.
		{"/api/export/markdown", true},
		{"/api/export/html", true},
		{"/api/export/pdf", true},
		{"/api/export/compare", false},
		{"/api/export/save", false},
		{"/api/export/pdf/extra", false},
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
		// The knowledge re-index commits page by page (R-7) and the upload
		// embeds the whole document through the provider before writing
		// (R-11): both outside the request transaction, whose writes open
		// their own short RLS transactions. Their siblings keep theirs.
		{"/api/orgs/org-1/knowledge/reindex", true},
		{"/api/orgs/org-1/knowledge/upload", true},
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

// controlWriter is a ResponseWriter that supports the per-request controls
// http.ResponseController looks up (as net/http's own writer does) and
// records each call.
type controlWriter struct {
	*httptest.ResponseRecorder
	writeDeadline time.Time
	readDeadline  time.Time
	fullDuplex    bool
}

func (c *controlWriter) SetWriteDeadline(d time.Time) error { c.writeDeadline = d; return nil }
func (c *controlWriter) SetReadDeadline(d time.Time) error  { c.readDeadline = d; return nil }
func (c *controlWriter) EnableFullDuplex() error            { c.fullDuplex = true; return nil }

// TestStatusRecorder_ResponseControllerReachesInnerWriter: the RLS
// statusRecorder had no Unwrap, so http.ResponseController stopped at it and
// every deadline / full-duplex call returned http.ErrNotSupported even though
// the writer underneath supports them — an RLS-wrapped streaming handler
// could not lift the server's WriteTimeout. The controls must pass through,
// and the recorder must still see the status written afterwards (it decides
// commit vs rollback).
func TestStatusRecorder_ResponseControllerReachesInnerWriter(t *testing.T) {
	inner := &controlWriter{ResponseRecorder: httptest.NewRecorder()}
	rec := &statusRecorder{ResponseWriter: inner, status: http.StatusOK}
	rc := http.NewResponseController(rec)

	deadline := time.Now().Add(time.Minute)
	if err := rc.SetWriteDeadline(deadline); err != nil {
		t.Fatalf("SetWriteDeadline through statusRecorder: %v (ErrNotSupported = the controller cannot unwrap it)", err)
	}
	if !inner.writeDeadline.Equal(deadline) {
		t.Errorf("inner write deadline = %v, want %v", inner.writeDeadline, deadline)
	}
	if err := rc.SetReadDeadline(deadline); err != nil {
		t.Fatalf("SetReadDeadline through statusRecorder: %v", err)
	}
	if !inner.readDeadline.Equal(deadline) {
		t.Errorf("inner read deadline = %v, want %v", inner.readDeadline, deadline)
	}
	if err := rc.EnableFullDuplex(); err != nil {
		t.Fatalf("EnableFullDuplex through statusRecorder: %v", err)
	}
	if !inner.fullDuplex {
		t.Error("EnableFullDuplex did not reach the inner writer")
	}

	rec.WriteHeader(http.StatusConflict)
	if rec.status != http.StatusConflict || inner.Code != http.StatusConflict {
		t.Errorf("status after controller calls: recorder %d, inner %d, want %d", rec.status, inner.Code, http.StatusConflict)
	}
	if err := rc.Flush(); err != nil {
		t.Fatalf("Flush through statusRecorder: %v", err)
	}
	if !inner.Flushed {
		t.Error("Flush did not reach the inner writer")
	}

	// The same through net/http's real writer, which is what the RLS
	// middleware wraps in production.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		rec := &statusRecorder{ResponseWriter: w, status: http.StatusOK}
		if err := http.NewResponseController(rec).SetWriteDeadline(time.Now().Add(time.Minute)); err != nil {
			http.Error(rec, err.Error(), http.StatusInternalServerError)
			return
		}
		rec.WriteHeader(http.StatusNoContent)
	}))
	defer srv.Close()
	resp, err := srv.Client().Get(srv.URL)
	if err != nil {
		t.Fatalf("GET: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNoContent {
		body, _ := io.ReadAll(resp.Body)
		t.Errorf("real server: SetWriteDeadline through statusRecorder failed: %d %s", resp.StatusCode, body)
	}
}
