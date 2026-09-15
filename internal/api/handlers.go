package api

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"html"
	"net/http"
	"os"
	"strings"
)

// serveStatic serves the SPA bundle with sensible caching + a Content-
// Security-Policy header on HTML responses, and rejects directory-listing
// requests. The path argument is the request path (URL.Path).
func (rt *Router) serveStatic(w http.ResponseWriter, r *http.Request, path string) {
	// Source maps are never served, whatever is on disk.
	//
	// vite builds with sourcemap:'hidden', which only omits the
	// //# sourceMappingURL comment — the .map files are still emitted into
	// dist/, and this handler serves anything under staticDir, so
	// GET /assets/index-<hash>.js.map returned a map with sourcesContent
	// populated: the application's complete original TypeScript, to anyone who
	// can reach the app. "hidden" stops a browser from fetching them
	// automatically; it does not stop a fetch. The Dockerfile also strips them
	// from the image, but that only covers the image — a compose stack or a
	// PAD_STATIC_DIR pointed at a local build has the same exposure, so the
	// guard belongs here too.
	if strings.HasSuffix(path, ".map") {
		http.NotFound(w, r)
		return
	}

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
		// Content-Security-Policy for the SPA shell.
		//   - script-src: the inline theme-bootstrap script in index.html is
		//     hash-pinned (no 'unsafe-inline'); bundled scripts come from 'self'.
		//   - style-src: 'unsafe-inline' is retained because the SPA and its
		//     third-party components (graph/toast/editor libs) inject <style>
		//     at runtime. Removing it without a per-request nonce scheme breaks
		//     the UI in ways unit tests can't detect, so it is a deliberate
		//     trade-off documented here rather than an oversight.
		//   - object-src 'none': there are no <object>/<embed> payloads, so
		//     legacy plugin content is blocked outright.
		w.Header().Set("Content-Security-Policy",
			"default-src 'self'; "+
				"script-src 'self' 'sha256-ukxiLLS3A6HuiM7piLMSGXuqzQQJAY0uuePIfYP+vdA='; "+
				"style-src 'self' 'unsafe-inline'; "+
				"img-src 'self' data:; "+
				"font-src 'self'; "+
				"connect-src 'self' ws: wss:; "+
				"object-src 'none'; "+
				"frame-ancestors 'none'; "+
				"base-uri 'none'; "+
				"form-action 'self'")
	} else if strings.HasPrefix(path, "/assets/") {
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}

	if isFallback && path == "/shared" {
		// The share viewer is an SPA route, so this is always the index.html
		// fallback. Give link unfurlers (Slack, Teams, iMessage) a real
		// preview by injecting the flow name into <title> + og:* meta —
		// best-effort; a bad/missing token serves the unmodified shell.
		if rt.serveSharedShell(w, r, indexPath) {
			return
		}
	}
	// #nosec G703 -- servedFile is either the constant indexPath or staticDir+path
	// where path already round-tripped through http.Dir.Open above, which rejects
	// traversal; a miss falls back to indexPath. The annotation sits on THIS line
	// deliberately: it previously sat (twice) on the `if isFallback` statement
	// above, where it applied to nothing, which is why it stopped suppressing the
	// moment gosec renumbered the rule.
	http.ServeFile(w, r, servedFile)
}

// serveSharedShell serves index.html with per-share metadata injected when the
// /shared route carries a resolvable token. Returns false when injection
// isn't possible (no token, storage miss, read error) so the caller falls
// through to the plain shell.
func (rt *Router) serveSharedShell(w http.ResponseWriter, r *http.Request, indexPath string) bool {
	token := r.URL.Query().Get("token")
	if token == "" {
		return false
	}
	analysis := rt.handlers.Analysis
	if analysis == nil || analysis.backend == nil || analysis.flowSvc == nil {
		return false
	}
	hash := sha256.Sum256([]byte(token))
	st, err := analysis.backend.GetShareTokenByHash(r.Context(), hex.EncodeToString(hash[:]))
	if err != nil {
		return false
	}
	doc, err := analysis.flowSvc.DocProvider().ResolveDoc(r.Context(), st.FlowID)
	if err != nil || doc == nil {
		return false
	}

	raw, err := os.ReadFile(indexPath) // #nosec G304 -- built from the configured static dir
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
