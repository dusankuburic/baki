package api

import (
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"pad-analyzer/internal/storage/filesystem"
)

// TestServeHTTP_SecurityHeadersOnEveryResponse verifies the always-on
// headers that defend against XFO, MIME-sniffing, referer leakage, and
// the cross-origin isolation primitives. These should be on **every**
// response regardless of route or auth state.
func TestServeHTTP_SecurityHeadersOnEveryResponse(t *testing.T) {
	rt := newJWTTestRouter(t)
	// /healthz is public + always 200 so we have a stable response to
	// inspect headers on.
	rr := doRequestWithAuth(t, rt, http.MethodGet, "/healthz", "", nil)

	cases := []struct {
		header string
		want   string // exact match; use "" to assert just that the header is set
	}{
		{"X-Content-Type-Options", "nosniff"},
		{"X-Frame-Options", "DENY"},
		{"Referrer-Policy", "strict-origin-when-cross-origin"},
		{"Permissions-Policy", ""},
		{"Cross-Origin-Opener-Policy", "same-origin"},
		{"Cross-Origin-Resource-Policy", "same-origin"},
		{"Strict-Transport-Security", ""}, // HSTS set whenever jwtEnabled
	}
	for _, c := range cases {
		got := rr.Header().Get(c.header)
		if c.want != "" {
			if got != c.want {
				t.Errorf("%s: got %q, want %q", c.header, got, c.want)
			}
		} else if got == "" {
			t.Errorf("%s: expected non-empty value", c.header)
		}
	}

	// Permissions-Policy must list the disabled APIs so we know the
	// directive is actually restrictive, not just present-but-empty.
	if pp := rr.Header().Get("Permissions-Policy"); !strings.Contains(pp, "geolocation=()") {
		t.Errorf("Permissions-Policy missing geolocation restriction; got %q", pp)
	}
}

// TestServeHTTP_StripsDefaultServerHeader verifies that the default Go
// `Server: Go-http-server/...` header is removed. Fingerprinting the
// runtime + version is a minor info-disclosure we don't need.
func TestServeHTTP_StripsDefaultServerHeader(t *testing.T) {
	rt := newJWTTestRouter(t)
	rr := doRequestWithAuth(t, rt, http.MethodGet, "/healthz", "", nil)
	// Go sets the header even when we set it to empty string explicitly,
	// in some versions. The test accepts either fully-absent or an empty
	// value — both meet the intent (no version disclosure).
	if v := rr.Header().Get("Server"); v != "" && strings.Contains(strings.ToLower(v), "go") {
		t.Errorf("Server header leaks Go runtime: %q", v)
	}
}

// makeStaticRouter wires a router with a temp static directory so the
// static-file path can be exercised without depending on a built frontend
// in the test environment.
func makeStaticRouter(t *testing.T, staticDir string) *Router {
	t.Helper()
	fs, _ := filesystem.NewLocalStorageBackend(t.TempDir())
	rt := newTestRouter(fs, true)
	rt.staticDir = staticDir
	return rt
}

// TestServeStatic_CSPOnIndexHtml verifies the HTML response carries a
// Content-Security-Policy. Without CSP, an XSS in the SPA bundle could
// exfiltrate the access token held in memory.
func TestServeStatic_CSPOnIndexHtml(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"),
		[]byte("<html><body>app</body></html>"), 0644); err != nil {
		t.Fatal(err)
	}
	rt := makeStaticRouter(t, dir)

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/some/route", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("SPA fallback: expected 200, got %d", rr.Code)
	}
	csp := rr.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("expected Content-Security-Policy on SPA HTML response")
	}
	for _, expected := range []string{
		"default-src 'self'",
		"frame-ancestors 'none'",
		"base-uri 'none'",
		"object-src 'none'",
	} {
		if !strings.Contains(csp, expected) {
			t.Errorf("CSP missing %q. Got: %s", expected, csp)
		}
	}
	if cc := rr.Header().Get("Cache-Control"); !strings.Contains(cc, "no-cache") {
		t.Errorf("index.html Cache-Control: want no-cache, got %q", cc)
	}
}

// TestServeStatic_ImmutableCacheOnHashedAssets verifies that hashed bundle
// paths under /assets/ get a long-cache header, which lets the browser
// avoid revalidating on every page load.
func TestServeStatic_ImmutableCacheOnHashedAssets(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "assets"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "assets", "index-AbCd12.js"),
		[]byte("// bundle"), 0644); err != nil {
		t.Fatal(err)
	}
	rt := makeStaticRouter(t, dir)

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/assets/index-AbCd12.js", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("/assets/* asset: expected 200, got %d", rr.Code)
	}
	cc := rr.Header().Get("Cache-Control")
	if !strings.Contains(cc, "immutable") || !strings.Contains(cc, "max-age=31536000") {
		t.Errorf("hashed asset Cache-Control: want immutable max-age=31536000, got %q", cc)
	}
}

// TestServeStatic_MissingAssetReturns404 verifies that a missing build artifact
// under /assets/ returns 404 rather than the SPA index.html. Serving index.html
// for a .js/.css request returns a text/html body the browser rejects with a
// strict-MIME error — the symptom when a redeploy changes content hashes while a
// stale tab requests an old chunk.
func TestServeStatic_MissingAssetReturns404(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"),
		[]byte("<html>app</html>"), 0644); err != nil {
		t.Fatal(err)
	}
	rt := makeStaticRouter(t, dir)

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/assets/old-hash-DEADBEEF.js", "", nil)
	if rr.Code != http.StatusNotFound {
		t.Fatalf("missing /assets/* file: expected 404, got %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); strings.Contains(ct, "text/html") {
		t.Errorf("missing asset must not return HTML, got Content-Type %q", ct)
	}

	// A non-asset client route still falls back to the SPA index.html.
	rr2 := doRequestWithAuth(t, rt, http.MethodGet, "/dashboard", "", nil)
	if rr2.Code != http.StatusOK {
		t.Errorf("client route fallback: expected 200, got %d", rr2.Code)
	}
}

// TestServeStatic_DirectoryListingRefused verifies that requests for a
// directory without an explicit index file get the SPA fallback rather
// than a directory listing, which would leak server-side layout.
func TestServeStatic_DirectoryListingRefused(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"),
		[]byte("<html>app</html>"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(dir, "secret-dir"), 0755); err != nil {
		t.Fatal(err)
	}
	// Put a file inside the directory but NO index.html — Go's default
	// http.FileServer would render a listing here.
	if err := os.WriteFile(filepath.Join(dir, "secret-dir", "private.txt"),
		[]byte("you can't see this filename in a listing"), 0644); err != nil {
		t.Fatal(err)
	}
	rt := makeStaticRouter(t, dir)

	rr := doRequestWithAuth(t, rt, http.MethodGet, "/secret-dir/", "", nil)
	if rr.Code != http.StatusOK {
		t.Fatalf("dir request: expected 200 (SPA fallback), got %d", rr.Code)
	}
	body := rr.Body.String()
	if strings.Contains(body, "private.txt") {
		t.Errorf("directory listing leaked filenames: %s", body)
	}
	if !strings.Contains(body, "<html>") {
		t.Errorf("expected SPA index.html fallback, got %.200s", body)
	}
}
