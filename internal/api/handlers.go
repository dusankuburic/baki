package api

import (
	"bytes"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"html"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"pad-analyzer/internal/api/middleware"
)

// indexScriptCSPHash derives the CSP pin for the SPA shell's inline theme-bootstrap script from the index.html actually served (computed once; the old hardcoded pin drifted from the build).
// Returns "" when the script cannot be located, leaving script-src at 'self' only.
func (rt *Router) indexScriptCSPHash(indexPath string) string {
	rt.indexCSPOnce.Do(func() {
		data, err := os.ReadFile(indexPath) // #nosec G304 -- build artifact path from config
		if err != nil {
			return
		}
		// The first plain <script> tag is the theme bootstrap; every other script in the built
		// shell carries attributes (type/src) and loads via 'self'.
		const openTag, closeTag = "<script>", "</script>"
		start := bytes.Index(data, []byte(openTag))
		if start < 0 {
			return
		}
		start += len(openTag)
		end := bytes.Index(data[start:], []byte(closeTag))
		if end < 0 {
			return
		}
		// The HTML tokenizer normalizes CRLF/CR to LF before the CSP engine
		// hashes an inline script, so the pin must hash the normalized text.
		body := bytes.ReplaceAll(data[start:start+end], []byte("\r\n"), []byte("\n"))
		body = bytes.ReplaceAll(body, []byte("\r"), []byte("\n"))
		sum := sha256.Sum256(body)
		// CSP source grammar requires the quotes: 'sha256-<b64>'.
		rt.indexCSPHash = "'sha256-" + base64.StdEncoding.EncodeToString(sum[:]) + "'"
	})
	return rt.indexCSPHash
}

// serveStatic serves the SPA bundle with sensible caching + a Content-Security-Policy header on HTML responses, and rejects directory-listing requests. The path argument is the request path (URL.Path).
func (rt *Router) serveStatic(w http.ResponseWriter, r *http.Request, path string) {
	// Source maps are never served: vite's sourcemap:'hidden' only omits the sourceMappingURL comment — the .map files are still emitted, and serving one exposed the complete
	// original TypeScript. The Dockerfile strips them, but a PAD_STATIC_DIR at a local build has the same exposure, so the guard belongs here.
	if strings.HasSuffix(path, ".map") {
		http.NotFound(w, r)
		return
	}

	indexPath := rt.staticDir + "/index.html"
	servedFile := rt.staticDir + path
	isFallback := false

	fs := http.Dir(rt.staticDir)
	var info os.FileInfo
	if path != "/" && path != "" {
		f, err := fs.Open(path)
		if err != nil {
			isFallback = true
			servedFile = indexPath
		} else {
			var statErr error
			info, statErr = f.Stat()
			_ = f.Close()
			if statErr != nil || info.IsDir() {
				isFallback = true
				servedFile = indexPath
			}
		}
	} else {
		isFallback = true
		servedFile = indexPath
	}

	// Never serve the SPA index.html in place of a missing /assets/ artifact: those are content-hashed, so a miss means a stale deploy's old hash, and a text/html
	// fallback for a .js/.css request is a strict-MIME error. A 404 lets the client reload the new index.html.
	if isFallback && strings.HasPrefix(path, "/assets/") {
		http.NotFound(w, r)
		return
	}

	if isFallback || strings.HasSuffix(path, "/index.html") {
		w.Header().Set("Cache-Control", "no-cache")
		// CSP for the SPA shell: script-src pins the inline theme-bootstrap by a runtime-derived hash (no drift from the build) and takes bundled scripts from 'self'.
		// style-src keeps 'unsafe-inline' deliberately — the SPA and its libs inject <style> at runtime. object-src 'none' blocks legacy plugin content outright.
		scriptSrc := "'self'"
		if pin := rt.indexScriptCSPHash(indexPath); pin != "" {
			scriptSrc += " " + pin
		}
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src "+scriptSrc+"; "+
				"style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data: blob:; "+
				"font-src 'self'; "+
				"connect-src 'self' ws: wss:; "+
				"object-src 'none'; "+
				"frame-ancestors 'none'; "+
				"base-uri 'none'; "+
				"form-action 'self'")
	} else if strings.HasPrefix(path, "/assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else if path == "/sw.js" || path == "/manifest.json" || path == "/offline.html" {
		// The service worker must be revalidated every load: a cached worker
		// keeps serving the OLD shell (and its caches) for its whole lifetime,
		// so a security-fix deploy would only reach browsers after the HTTP
		// cache expires. Its dependencies ride the hashed /assets/ entries.
		w.Header().Set("Cache-Control", "no-cache")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}

	if isFallback && path == "/shared" {
		// The share viewer is an SPA route, so this is always the index.html fallback. Give link unfurlers (Slack, Teams, iMessage) a real preview by injecting
		// the flow name into <title> + og:* meta — best-effort; a bad/missing token serves the unmodified shell.
		if rt.serveSharedShell(w, r, indexPath) {
			return
		}
	}
	if !isFallback && !strings.HasSuffix(path, "/index.html") && servePrecompressed(w, r, fs, path, info) {
		return
	}
	// #nosec G703 -- servedFile is either the constant indexPath or staticDir+path where path
	// already round-tripped through http.Dir.Open above, which rejects traversal. The annotation
	// sits on THIS line deliberately: it previously sat on the `if isFallback` statement above,
	// where it applied to nothing, which is why it stopped suppressing when gosec renumbered.
	http.ServeFile(w, r, servedFile)
}

// precompressedVariants are the build-time encodings of a static file, as the frontend build writes them next to it (frontend/scripts/precompress.mjs): app.js → app.js.br, app.js.gz.
var precompressedVariants = []struct{ encoding, suffix string }{
	{"br", ".br"},
	{"gzip", ".gz"},
}

// servePrecompressed serves a precompressed sibling of the static file at path when the client accepts its encoding, and reports whether it did (on false the caller serves the file itself).
// Per-request compression is unaffordable (the main chunk alone costs 26 ms of brotli CPU), so the build compresses once at maximum quality; the response is the original's in every other respect (Compress skips it: it already has a Content-Encoding). Range/If-Range are dropped — an If-Range resume could splice bytes of one representation onto the other — and a sibling older than the file is ignored, so a rebuild cannot serve stale bytes.
func servePrecompressed(w http.ResponseWriter, r *http.Request, fs http.Dir, path string, info os.FileInfo) bool {
	if info == nil {
		return false
	}
	ctype := mime.TypeByExtension(filepath.Ext(path))
	if ctype == "" {
		return false // ServeFile would sniff the content; leave it to it
	}
	var offered []string
	for _, v := range precompressedVariants {
		st, err := os.Stat(filepath.Join(string(fs), filepath.FromSlash(path+v.suffix))) // #nosec G304 -- path round-tripped through http.Dir.Open in serveStatic
		if err == nil && st.Mode().IsRegular() && !st.ModTime().Before(info.ModTime()) {
			offered = append(offered, v.encoding)
		}
	}
	if len(offered) == 0 {
		return false
	}
	// The response now depends on Accept-Encoding, whichever way it goes.
	addVaryAcceptEncoding(w.Header())
	enc := middleware.NegotiateEncoding(r.Header.Get("Accept-Encoding"), offered...)
	if enc == "" {
		return false
	}
	suffix := ".gz"
	if enc == "br" {
		suffix = ".br"
	}
	f, err := fs.Open(path + suffix)
	if err != nil {
		return false
	}
	defer func() { _ = f.Close() }()
	st, err := f.Stat()
	if err != nil {
		return false
	}

	h := w.Header()
	h.Set("Content-Type", ctype)
	h.Set("Content-Encoding", enc)
	// ServeContent leaves Content-Length unset when a Content-Encoding is set
	// (it allows for a wrapper that encodes on the fly); this body is final.
	h.Set("Content-Length", strconv.FormatInt(st.Size(), 10))
	noRange := r.Clone(r.Context())
	noRange.Header.Del("Range")
	noRange.Header.Del("If-Range")
	http.ServeContent(noRangesWriter{w}, noRange, path, info.ModTime(), f)
	return true
}

// addVaryAcceptEncoding adds Accept-Encoding to Vary unless it is already
// there (Compress adds it to every response it wraps).
func addVaryAcceptEncoding(h http.Header) {
	for _, v := range h.Values("Vary") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "Accept-Encoding") {
				return
			}
		}
	}
	h.Add("Vary", "Accept-Encoding")
}

// noRangesWriter drops the "Accept-Ranges: bytes" ServeContent always sets,
// for a response that does not serve ranges (servePrecompressed).
type noRangesWriter struct{ http.ResponseWriter }

func (w noRangesWriter) WriteHeader(code int) {
	w.Header().Del("Accept-Ranges")
	w.ResponseWriter.WriteHeader(code)
}

func (w noRangesWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

// serveSharedShell serves index.html with per-share metadata injected when the /shared route carries a resolvable token. Returns false when injection isn't possible (no token, storage miss, read error) so the caller falls through to the plain shell.
func (rt *Router) serveSharedShell(w http.ResponseWriter, r *http.Request, indexPath string) bool {
	token := r.URL.Query().Get("token")
	if token == "" || rt.shares == nil {
		return false
	}
	doc := rt.shares.LookupSharedFlowHeader(r.Context(), token)
	if doc == nil {
		return false
	}

	raw, err := readIndexShell(indexPath)
	if err != nil {
		return false
	}

	name := html.EscapeString(doc.Name)
	shell := strings.Replace(string(raw), "<title>PAD Analyzer</title>",
		fmt.Sprintf("<title>%s — PAD Analyzer</title>", name), 1)
	meta := fmt.Sprintf(
		`<meta property="og:title" content="%s"/>`+
			`<meta property="og:description" content="Shared read-only analysis report for flow %s."/>`+
			`<meta property="og:type" content="website"/>`+
			`<meta name="twitter:card" content="summary"/>`,
		name, name)
	shell = strings.Replace(shell, "<title>", meta+"<title>", 1)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Content-Length", fmt.Sprintf("%d", len(shell)))
	_, _ = w.Write([]byte(shell))
	return true
}

// indexShellCache holds the bytes of each index.html serveSharedShell served, keyed by path: the shell is an immutable build artifact, so it is read once rather
// than per share-link hit. Read errors are not cached.
var indexShellCache sync.Map

// readIndexShell returns the bytes of the SPA shell at indexPath, from the
// cache after the first successful read.
func readIndexShell(indexPath string) ([]byte, error) {
	if v, ok := indexShellCache.Load(indexPath); ok {
		return v.([]byte), nil
	}
	raw, err := os.ReadFile(indexPath) // #nosec G304 -- built from the configured static dir
	if err != nil {
		return nil, err
	}
	v, _ := indexShellCache.LoadOrStore(indexPath, raw)
	return v.([]byte), nil
}
