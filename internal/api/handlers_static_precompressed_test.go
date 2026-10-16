package api

import (
	"bytes"
	"compress/gzip"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/andybalholm/brotli"

	"pad-analyzer/internal/api/middleware"
)

// precompressedAsset is the body of the hashed bundle chunk the tests serve.
var precompressedAsset = []byte(strings.Repeat("export const answer = () => 42; // padding\n", 64))

// precompressedRouter builds a router over a temp static dir holding the SPA
// shell and one hashed chunk with .br and .gz siblings, as the frontend build
// writes them (same mtime as the original).
func precompressedRouter(t *testing.T) (*Router, string) {
	t.Helper()
	dir := t.TempDir()
	assets := filepath.Join(dir, "assets")
	if err := os.MkdirAll(assets, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, data []byte) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(dir, name), data, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	write("index.html", []byte("<!doctype html><html><head><title>PAD Analyzer</title></head><body></body></html>"))
	write("assets/app-abc123.js", precompressedAsset)
	write("assets/app-abc123.js.br", encodeBrotli(t, precompressedAsset))
	write("assets/app-abc123.js.gz", encodeGzip(t, precompressedAsset))
	mtime := time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC)
	for _, name := range []string{"app-abc123.js", "app-abc123.js.br", "app-abc123.js.gz"} {
		if err := os.Chtimes(filepath.Join(assets, name), mtime, mtime); err != nil {
			t.Fatal(err)
		}
	}
	rt := newTestRouter(nil, false)
	rt.staticDir = dir
	return rt, dir
}

func encodeBrotli(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w := brotli.NewWriterLevel(&buf, brotli.BestCompression)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func encodeGzip(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	w, _ := gzip.NewWriterLevel(&buf, gzip.BestCompression)
	if _, err := w.Write(b); err != nil {
		t.Fatal(err)
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func getStatic(h http.Handler, method, target string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, target, nil)
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, req)
	return rr
}

// varyAcceptEncodingCount counts Accept-Encoding among the Vary tokens (CORS
// adds Origin alongside).
func varyAcceptEncodingCount(h http.Header) int {
	n := 0
	for _, v := range h.Values("Vary") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "Accept-Encoding") {
				n++
			}
		}
	}
	return n
}

func decodeEncodedBody(t *testing.T, enc string, body []byte) []byte {
	t.Helper()
	var r io.Reader
	switch enc {
	case "br":
		r = brotli.NewReader(bytes.NewReader(body))
	case "gzip":
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			t.Fatalf("gzip reader: %v", err)
		}
		r = zr
	default:
		return body
	}
	out, err := io.ReadAll(r)
	if err != nil {
		t.Fatalf("decode %s: %v", enc, err)
	}
	return out
}

// TestServeStatic_ServesPrecompressedAsset: a hashed asset is served from its
// build-time sibling instead of being compressed per request (26 ms of CPU
// for the main chunk with brotli, measured), with the original's
// Content-Type, Last-Modified and immutable Cache-Control.
func TestServeStatic_ServesPrecompressedAsset(t *testing.T) {
	rt, dir := precompressedRouter(t)
	identity := getStatic(rt, http.MethodGet, "/assets/app-abc123.js", nil)
	if identity.Code != http.StatusOK || !bytes.Equal(identity.Body.Bytes(), precompressedAsset) {
		t.Fatalf("identity request: status %d, %d bytes", identity.Code, identity.Body.Len())
	}

	for _, tc := range []struct {
		accept, want, sibling string
	}{
		{"gzip, deflate, br, zstd", "br", ".br"},
		{"gzip", "gzip", ".gz"},
		{"br;q=0, gzip", "gzip", ".gz"},
		{"gzip;q=1, br;q=0.5", "gzip", ".gz"},
		{"*", "br", ".br"},
	} {
		t.Run(tc.accept, func(t *testing.T) {
			rr := getStatic(rt, http.MethodGet, "/assets/app-abc123.js", map[string]string{"Accept-Encoding": tc.accept})
			if rr.Code != http.StatusOK {
				t.Fatalf("status = %d", rr.Code)
			}
			if ce := rr.Header().Get("Content-Encoding"); ce != tc.want {
				t.Fatalf("Content-Encoding = %q, want %q", ce, tc.want)
			}
			onDisk, _ := os.ReadFile(filepath.Join(dir, "assets", "app-abc123.js"+tc.sibling))
			if !bytes.Equal(rr.Body.Bytes(), onDisk) {
				t.Errorf("body is not the %s sibling's bytes", tc.sibling)
			}
			if got := decodeEncodedBody(t, tc.want, rr.Body.Bytes()); !bytes.Equal(got, precompressedAsset) {
				t.Error("decoded body differs from the original asset")
			}
			if got, want := rr.Header().Get("Content-Type"), identity.Header().Get("Content-Type"); got != want || got == "" {
				t.Errorf("Content-Type = %q, want the original's %q", got, want)
			}
			if got, want := rr.Header().Get("Last-Modified"), identity.Header().Get("Last-Modified"); got != want || got == "" {
				t.Errorf("Last-Modified = %q, want the original's %q", got, want)
			}
			if cc := rr.Header().Get("Cache-Control"); cc != "public, max-age=31536000, immutable" {
				t.Errorf("Cache-Control = %q, want the immutable asset policy", cc)
			}
			if cl := rr.Header().Get("Content-Length"); cl != strconv.Itoa(len(onDisk)) {
				t.Errorf("Content-Length = %q, want %d", cl, len(onDisk))
			}
			if n := varyAcceptEncodingCount(rr.Header()); n != 1 {
				t.Errorf("Vary = %q, want Accept-Encoding once", rr.Header().Values("Vary"))
			}
		})
	}
}

// TestServeStatic_PrecompressedNotAcceptedServesIdentity: a client that
// accepts none of the encodings on disk gets the file itself, as before, with
// ranges still available.
func TestServeStatic_PrecompressedNotAcceptedServesIdentity(t *testing.T) {
	rt, _ := precompressedRouter(t)
	for _, accept := range []string{"", "identity", "br;q=0, gzip;q=0", "deflate", "*;q=0"} {
		rr := getStatic(rt, http.MethodGet, "/assets/app-abc123.js", map[string]string{"Accept-Encoding": accept})
		if ce := rr.Header().Get("Content-Encoding"); ce != "" {
			t.Errorf("Accept-Encoding %q: Content-Encoding = %q, want identity", accept, ce)
		}
		if !bytes.Equal(rr.Body.Bytes(), precompressedAsset) {
			t.Errorf("Accept-Encoding %q: body is not the original asset", accept)
		}
		if rr.Header().Get("Accept-Ranges") != "bytes" {
			t.Errorf("Accept-Encoding %q: identity response lost Accept-Ranges", accept)
		}
	}
	rr := getStatic(rt, http.MethodGet, "/assets/app-abc123.js", map[string]string{"Range": "bytes=0-9"})
	if rr.Code != http.StatusPartialContent || rr.Body.String() != string(precompressedAsset[:10]) {
		t.Errorf("identity Range: status %d body %q, want 206 with the first 10 bytes", rr.Code, rr.Body.String())
	}
}

// TestServeStatic_PrecompressedIgnoresRange: ranges are not served from an
// encoded variant (they would share Last-Modified with the identity bytes, so
// an If-Range resume could splice the two). The full encoded body comes back
// with 200, and Accept-Ranges is not advertised for it.
func TestServeStatic_PrecompressedIgnoresRange(t *testing.T) {
	rt, dir := precompressedRouter(t)
	onDisk, _ := os.ReadFile(filepath.Join(dir, "assets", "app-abc123.js.br"))
	rr := getStatic(rt, http.MethodGet, "/assets/app-abc123.js", map[string]string{
		"Accept-Encoding": "br",
		"Range":           "bytes=0-9",
		"If-Range":        time.Date(2026, 9, 1, 12, 0, 0, 0, time.UTC).Format(http.TimeFormat),
	})
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (ranges disabled on encoded variants)", rr.Code)
	}
	if rr.Header().Get("Content-Range") != "" || rr.Header().Get("Accept-Ranges") != "" {
		t.Errorf("Content-Range %q / Accept-Ranges %q on an encoded response",
			rr.Header().Get("Content-Range"), rr.Header().Get("Accept-Ranges"))
	}
	if !bytes.Equal(rr.Body.Bytes(), onDisk) {
		t.Error("body is not the full brotli sibling")
	}
}

// TestServeStatic_PrecompressedHeadAndConditional: HEAD answers with the
// encoded variant's headers and no body; If-Modified-Since revalidates
// against the original's mtime.
func TestServeStatic_PrecompressedHeadAndConditional(t *testing.T) {
	rt, dir := precompressedRouter(t)
	onDisk, _ := os.ReadFile(filepath.Join(dir, "assets", "app-abc123.js.br"))
	head := getStatic(rt, http.MethodHead, "/assets/app-abc123.js", map[string]string{"Accept-Encoding": "br"})
	if head.Code != http.StatusOK || head.Body.Len() != 0 {
		t.Fatalf("HEAD: status %d, %d body bytes", head.Code, head.Body.Len())
	}
	if head.Header().Get("Content-Encoding") != "br" || head.Header().Get("Content-Length") != strconv.Itoa(len(onDisk)) {
		t.Errorf("HEAD: Content-Encoding %q Content-Length %q, want br / %d",
			head.Header().Get("Content-Encoding"), head.Header().Get("Content-Length"), len(onDisk))
	}

	cond := getStatic(rt, http.MethodGet, "/assets/app-abc123.js", map[string]string{
		"Accept-Encoding":   "br",
		"If-Modified-Since": time.Date(2026, 9, 2, 0, 0, 0, 0, time.UTC).Format(http.TimeFormat),
	})
	if cond.Code != http.StatusNotModified || cond.Body.Len() != 0 {
		t.Errorf("If-Modified-Since: status %d, %d body bytes; want 304", cond.Code, cond.Body.Len())
	}
}

// TestServeStatic_PrecompressedNotRecompressed: behind middleware.Compress the
// precompressed body goes out once-encoded — Compress leaves a response that
// already has a Content-Encoding alone.
func TestServeStatic_PrecompressedNotRecompressed(t *testing.T) {
	rt, dir := precompressedRouter(t)
	onDisk, _ := os.ReadFile(filepath.Join(dir, "assets", "app-abc123.js.gz"))
	rr := getStatic(middleware.Compress(rt), http.MethodGet, "/assets/app-abc123.js", map[string]string{"Accept-Encoding": "gzip"})
	if ce := rr.Header().Values("Content-Encoding"); len(ce) != 1 || ce[0] != "gzip" {
		t.Fatalf("Content-Encoding = %q, want [gzip]", ce)
	}
	if !bytes.Equal(rr.Body.Bytes(), onDisk) {
		t.Error("body is not the gzip sibling's bytes — re-encoded by the middleware?")
	}
	if n := varyAcceptEncodingCount(rr.Header()); n != 1 {
		t.Errorf("Vary = %q, want Accept-Encoding once", rr.Header().Values("Vary"))
	}
}

// TestServeStatic_StaleOrForeignSiblingIgnored: a sibling older than the file
// (rebuilt without re-running precompression) is not served, and index.html is
// never served precompressed (its CSP pin and no-cache policy belong to the
// file itself).
func TestServeStatic_StaleOrForeignSiblingIgnored(t *testing.T) {
	rt, dir := precompressedRouter(t)
	older := time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC)
	for _, name := range []string{"app-abc123.js.br", "app-abc123.js.gz"} {
		if err := os.Chtimes(filepath.Join(dir, "assets", name), older, older); err != nil {
			t.Fatal(err)
		}
	}
	rr := getStatic(rt, http.MethodGet, "/assets/app-abc123.js", map[string]string{"Accept-Encoding": "br, gzip"})
	if ce := rr.Header().Get("Content-Encoding"); ce != "" || !bytes.Equal(rr.Body.Bytes(), precompressedAsset) {
		t.Errorf("stale sibling served: Content-Encoding %q", ce)
	}

	index, _ := os.ReadFile(filepath.Join(dir, "index.html"))
	if err := os.WriteFile(filepath.Join(dir, "index.html.br"), encodeBrotli(t, index), 0o600); err != nil {
		t.Fatal(err)
	}
	rr = getStatic(rt, http.MethodGet, "/", map[string]string{"Accept-Encoding": "br"})
	if ce := rr.Header().Get("Content-Encoding"); ce != "" || !bytes.Equal(rr.Body.Bytes(), index) {
		t.Errorf("index.html served precompressed: Content-Encoding %q", ce)
	}
	if rr.Header().Get("Cache-Control") != "no-cache" {
		t.Errorf("index.html Cache-Control = %q, want no-cache", rr.Header().Get("Cache-Control"))
	}
	rr = getStatic(rt, http.MethodGet, "/index.html", map[string]string{"Accept-Encoding": "br"})
	if ce := rr.Header().Get("Content-Encoding"); ce != "" {
		t.Errorf("/index.html served precompressed: Content-Encoding %q (status %d)", ce, rr.Code)
	}
}

// TestServeStatic_ServiceWorkerAndFriendsAreNoCache: the service worker (and
// its manifest / offline shell) must revalidate on every load — a cached
// worker keeps serving the old shell for its whole lifetime, so a
// security-fix deploy would only reach browsers after the HTTP cache expiry.
// Hashed /assets/ keep their immutable year.
func TestServeStatic_ServiceWorkerAndFriendsAreNoCache(t *testing.T) {
	rt, dir := precompressedRouter(t)
	for _, name := range []string{"sw.js", "manifest.json", "offline.html"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte("/*stub*/"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/sw.js", "/manifest.json", "/offline.html"} {
		w := httptest.NewRecorder()
		rt.ServeHTTP(w, httptest.NewRequest(http.MethodGet, path, nil))
		if w.Code != http.StatusOK {
			t.Fatalf("%s: status %d", path, w.Code)
		}
		if cc := w.Header().Get("Cache-Control"); cc != "no-cache" {
			t.Errorf("%s: Cache-Control %q, want no-cache", path, cc)
		}
	}

	w := httptest.NewRecorder()
	rt.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/assets/app-abc123.js", nil))
	if cc := w.Header().Get("Cache-Control"); !strings.Contains(cc, "immutable") {
		t.Errorf("hashed asset: Cache-Control %q, want immutable", cc)
	}
}
