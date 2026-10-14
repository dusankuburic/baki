package api

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// TestServeStaticCSPHashQuoted guards the CSP hash-pin syntax: hash sources
// must be emitted as 'sha256-<b64>' (quoted). An unquoted pin parses as a host
// source, the browser discards it as invalid, and the inline theme bootstrap
// is blocked on every page load — with nothing but a console warning.
func TestServeStaticCSPHashQuoted(t *testing.T) {
	dir := t.TempDir()
	const inline = "document.documentElement.dataset.cspTest = '1';"
	html := "<!doctype html><html><head><script>\n" + inline + "\n</script></head><body></body></html>"
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte(html), 0o600); err != nil {
		t.Fatalf("write index.html: %v", err)
	}

	rt := newTestRouter(nil, false)
	rt.staticDir = dir

	rr := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rt.serveStatic(rr, req, "/")

	csp := rr.Header().Get("Content-Security-Policy")
	if csp == "" {
		t.Fatal("no Content-Security-Policy header on the SPA shell")
	}
	m := regexp.MustCompile(`script-src ([^;]+);`).FindStringSubmatch(csp)
	if m == nil {
		t.Fatalf("no script-src directive in %q", csp)
	}
	for _, src := range strings.Fields(m[1]) {
		if strings.HasPrefix(src, "sha256-") {
			t.Errorf("hash source %q is not quoted — browsers treat an unquoted hash as an invalid host source and ignore it", src)
		}
		if !strings.HasPrefix(src, "'") || !strings.HasSuffix(src, "'") {
			t.Errorf("source %q is not single-quoted as the CSP grammar requires", src)
		}
	}
}

// TestServeStaticCSPAllowsAvatarObjectURLs guards img-src: uploaded avatars sit behind an authenticated route, so
// the SPA fetches the bytes and renders them from a blob: object URL (frontend/src/lib/avatarCache.ts). Without
// blob: in img-src the browser refuses every uploaded avatar and the UI quietly shows initials.
func TestServeStaticCSPAllowsAvatarObjectURLs(t *testing.T) {
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "index.html"), []byte("<!doctype html><html><body></body></html>"), 0o600); err != nil {
		t.Fatalf("write index.html: %v", err)
	}
	rt := newTestRouter(nil, false)
	rt.staticDir = dir

	rr := httptest.NewRecorder()
	rt.serveStatic(rr, httptest.NewRequest(http.MethodGet, "/", nil), "/")

	m := regexp.MustCompile(`img-src ([^;]+);`).FindStringSubmatch(rr.Header().Get("Content-Security-Policy"))
	if m == nil {
		t.Fatalf("no img-src directive in %q", rr.Header().Get("Content-Security-Policy"))
	}
	if !strings.Contains(" "+m[1]+" ", " blob: ") {
		t.Errorf("img-src %q lacks blob: — uploaded avatars render from object URLs", m[1])
	}
}
