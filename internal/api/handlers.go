package api

import (
	"net/http"
	"strings"
)

// serveStatic serves the SPA bundle with sensible caching + a Content-
// Security-Policy header on HTML responses, and rejects directory-listing
// requests. The path argument is the request path (URL.Path).
func (rt *Router) serveStatic(w http.ResponseWriter, r *http.Request, path string) {
	indexPath := rt.staticDir + "/index.html"
	servedFile := rt.staticDir + path
	isFallback := false

	fs := http.Dir(rt.staticDir)
	if path != "/" && path != "" {
		f, err := fs.Open(path)
		if err != nil {
			isFallback = true
			servedFile = indexPath
		} else {
			info, statErr := f.Stat()
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

	// Never serve the SPA index.html in place of a missing build artifact under
	// /assets/. Those files are content-hashed and immutable, so a miss means the
	// file genuinely isn't there — e.g. a browser running a stale index.html from
	// a previous deploy requests an old hash. Falling back to index.html returns a
	// text/html body for a .js/.css request, which the browser rejects with a
	// strict-MIME error ("Failed to load module script"). A 404 is the correct
	// answer and lets the client recover (reload to pick up the new index.html).
	if isFallback && strings.HasPrefix(path, "/assets/") {
		http.NotFound(w, r)
		return
	}

	if isFallback || strings.HasSuffix(path, "/index.html") {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self' 'sha256-ukxiLLS3A6HuiM7piLMSGXuqzQQJAY0uuePIfYP+vdA='; "+
				"style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; "+
				"font-src 'self'; "+
				"connect-src 'self' ws: wss:; "+
				"frame-ancestors 'none'; "+
				"base-uri 'none'; "+
				"form-action 'self'")
	} else if strings.HasPrefix(path, "/assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}

	// #nosec G703 -- path is validated by http.Dir.Open (rejects traversal) above;
	// any miss falls back to the constant index.html.
	http.ServeFile(w, r, servedFile)
}
