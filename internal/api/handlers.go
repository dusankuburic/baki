package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/logger"
)

func (rt *Router) dispatch(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	logger.Debug("dispatching request", "method", r.Method, "path", path)

	// Library routes: /api/library[/:id[/content]]
	if strings.HasPrefix(path, "/api/library") {
		remaining := strings.TrimPrefix(path, "/api/library")
		if remaining == "" || remaining == "/" {
			if r.Method == http.MethodGet {
				rt.handleLibraryList(w, r)
			} else if r.Method == http.MethodPost {
				rt.handleLibraryCreate(w, r)
			} else {
				http.NotFound(w, r)
			}
			return
		}
		// It's a sub-path like /api/library/123 or /api/library/123/content
		rt.handleLibraryItem(w, r)
		return
	}

	// Sharing routes: /api/flows/:flowId/collaborators[/:userId]
	if strings.HasPrefix(r.URL.Path, "/api/flows/") {
		rt.handleSharingRoute(w, r)
		return
	}

	// Org routes (REST): /api/orgs[/:id[/members[/:userId[/role]]]]
	if path == "/api/orgs" || strings.HasPrefix(path, "/api/orgs/") {
		rt.handleOrgsRoute(w, r)
		return
	}

	switch r.URL.Path {
	// --- Health ---
	case "/healthz":
		rt.handleLiveness(w, r)
	case "/readyz":
		rt.handleReadiness(w, r)
	case "/api/health":
		// Backwards-compatible alias for /readyz.
		rt.handleReadiness(w, r)

	// --- Metrics ---
	case "/metrics":
		// Prometheus exposition; should be reachable only from the scraper
		// (private network / sidecar). In a public-internet deployment,
		// drop this at the load balancer or move to a private listener.
		middleware.MetricsHandler().ServeHTTP(w, r)

	// --- System ---
	case "/api/system/settings":
		if r.Method == http.MethodGet {
			rt.handleGetSettings(w, r)
		} else {
			rt.handleUpdateSettings(w, r)
		}
	case "/api/system/info":
		rt.handleAppInfo(w, r)
	case "/api/system/log-error":
		rt.handleLogError(w, r)

	// --- Keys ---
	case "/api/keys/save":
		rt.handleSaveApiKey(w, r)
	case "/api/keys/has":
		rt.handleHasApiKey(w, r)
	case "/api/keys/delete":
		rt.handleDeleteApiKey(w, r)

	// --- Flow ---
	case "/api/flow/load-path":
		rt.handleLoadFlowFromPath(w, r)
	case "/api/flow/load-folder":
		rt.handleLoadFlowFolder(w, r)
	case "/api/flow/upload":
		rt.handleUploadFlow(w, r)
	case "/api/flow/recent":
		rt.handleRecentFiles(w, r)
	case "/api/flow/remove-recent":
		rt.handleRemoveRecentFile(w, r)
	case "/api/flow/clear-recent":
		rt.handleClearRecentFiles(w, r)
	case "/api/flow/reveal":
		rt.handleRevealInFileManager(w, r)
	case "/api/flow/search":
		rt.handleSearchFlow(w, r)
	case "/api/flow/source-files":
		rt.handleGetSourceFiles(w, r)
	case "/api/flow/read-sources":
		rt.handleReadSourceFiles(w, r)
	case "/api/flow/open-from-system":
		rt.handleOnFileOpenFromSystem(w, r)

	// --- Providers ---
	case "/api/providers/list":
		rt.handleListProviders(w, r)
	case "/api/providers/test":
		rt.handleTestProviderConnection(w, r)
	case "/api/providers/github/start":
		rt.handleStartGitHubAuth(w, r)
	case "/api/providers/github/poll":
		rt.handlePollGitHubAuth(w, r)
	case "/api/providers/github/revoke":
		rt.handleRevokeGitHubAuth(w, r)
	case "/api/providers/github/user":
		rt.handleGetGitHubUser(w, r)
	case "/api/providers/copilot/start":
		rt.handleStartCopilotAuth(w, r)
	case "/api/providers/copilot/poll":
		rt.handlePollCopilotAuth(w, r)
	case "/api/providers/copilot/revoke":
		rt.handleRevokeCopilotAuth(w, r)
	case "/api/providers/copilot/user":
		rt.handleGetCopilotUser(w, r)

	// --- Chat ---
	case "/api/chat/stream":
		rt.handleStreamChatMessage(w, r)
	case "/api/chat/begin":
		rt.handleBeginStream(w, r)
	case "/api/chat/cancel":
		rt.handleCancelStream(w, r)
	case "/api/chat/get":
		rt.handleGetConversation(w, r)
	case "/api/chat/save":
		rt.handleSaveConversation(w, r)
	case "/api/chat/clear":
		rt.handleClearConversation(w, r)
	case "/api/chat/export":
		rt.handleExportConversation(w, r)
	case "/api/chat/demo-remaining":
		rt.handleGetDemoRemaining(w, r)
	case "/api/chat/preview-context":
		rt.handlePreviewContext(w, r)
	case "/api/chat/suggested-prompts":
		rt.handleGetSuggestedPrompts(w, r)

	// --- Analysis ---
	case "/api/analysis/analyze":
		rt.handleAnalyzeFlow(w, r)
	case "/api/analysis/lineage":
		rt.handleGetVariableLineage(w, r)
	case "/api/analysis/graph":
		rt.handleGetExecutionGraph(w, r)
	case "/api/analysis/rules":
		rt.handleGetRules(w, r)
	case "/api/analysis/rule/enabled":
		rt.handleSetRuleEnabled(w, r)
	case "/api/analysis/rule/config":
		rt.handleUpdateRuleConfig(w, r)

	// --- Export ---
	case "/api/export/compare":
		rt.handleCompareCurrentWith(w, r)
	case "/api/export/markdown":
		rt.handleExportMarkdown(w, r)
	case "/api/export/pdf":
		rt.handleExportPDF(w, r)

	// --- Auth (JWT) ---
	case "/api/auth/register":
		rt.handleAuthRegister(w, r)
	case "/api/auth/login":
		rt.handleAuthLogin(w, r)
	case "/api/auth/refresh":
		rt.handleAuthRefresh(w, r)
	case "/api/auth/me":
		rt.handleAuthMe(w, r)
	case "/api/auth/logout":
		rt.handleAuthLogout(w, r)
	case "/api/auth/change-password":
		rt.handleAuthChangePassword(w, r)
	case "/api/ws-ticket":
		rt.handleWSTicket(w, r)

	// --- Admin ---
	case "/api/admin/users/list":
		if r.Method == http.MethodGet {
			rt.handleAdminUserList(w, r)
		} else {
			http.NotFound(w, r)
		}
	case "/api/admin/migration/start":
		if r.Method == http.MethodPost {
			rt.handleMigrationStart(w, r)
		} else {
			http.NotFound(w, r)
		}
	case "/api/admin/migration/status":
		if r.Method == http.MethodGet {
			rt.handleMigrationStatus(w, r)
		} else {
			http.NotFound(w, r)
		}

	default:
		if strings.HasPrefix(path, "/api/admin/users/") {
			rt.handleAdminUserRole(w, r)
			return
		}

		// If staticDir is set, serve frontend assets.
		if rt.staticDir != "" {
			// API routes that didn't match should return 404, not the SPA index.html.
			if strings.HasPrefix(path, "/api/") {
				http.NotFound(w, r)
				return
			}
			rt.serveStatic(w, r, path)
			return
		}

		http.NotFound(w, r)
	}
}

// serveStatic serves the SPA bundle with sensible caching + a Content-
// Security-Policy header on HTML responses, and rejects directory-listing
// requests. The path argument is the request path (URL.Path).
//
// Caching strategy:
//
//   - Hashed bundle assets under /assets/ (Vite emits e.g. /assets/index-AbCd12.js)
//     are content-addressed → safe to cache forever, `immutable` hints the
//     browser not to bother revalidating.
//   - index.html (and the SPA fallback) MUST use `no-cache` so a new
//     deployment doesn't get stuck behind the previous version's HTML
//     pointing at the previous version's bundle hashes.
//   - Everything else gets a moderate cache.
func (rt *Router) serveStatic(w http.ResponseWriter, r *http.Request, path string) {
	indexPath := rt.staticDir + "/index.html"
	servedFile := rt.staticDir + path
	isFallback := false

	// Existence + directory-listing guard. http.Dir.Open would serve a
	// directory listing if the directory had no index.html; we explicitly
	// refuse that to avoid leaking the static layout.
	fs := http.Dir(rt.staticDir)
	if path != "/" && path != "" {
		f, err := fs.Open(path)
		if err != nil {
			// Missing → SPA fallback to index.html (history-mode routing).
			isFallback = true
			servedFile = indexPath
		} else {
			info, statErr := f.Stat()
			_ = f.Close()
			if statErr != nil || info.IsDir() {
				// Directory request without an explicit index → fall back
				// to the SPA index. We never serve directory listings.
				isFallback = true
				servedFile = indexPath
			}
		}
	} else {
		isFallback = true
		servedFile = indexPath
	}

	// Cache-Control + (for HTML) Content-Security-Policy.
	if isFallback || strings.HasSuffix(path, "/index.html") {
		w.Header().Set("Cache-Control", "no-cache")
		// CSP on HTML only — JSON API responses don't need it. 'unsafe-inline'
		// on style-src is required by Vite's runtime CSS injection; the
		// theme-loading inline script hash is added to script-src.
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
		// Vite/webpack-style hashed bundles are content-addressed.
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	} else {
		// Other static files (logos, fonts shipped with old paths, etc.)
		// get a modest cache.
		w.Header().Set("Cache-Control", "public, max-age=3600")
	}

	http.ServeFile(w, r, servedFile)
}

// ---- Response helpers ----

var roleRank = map[auth.Role]int{
	auth.RoleAdmin:  40,
	auth.RoleMember: 30,
	auth.RoleViewer: 20,
	auth.RoleGuest:  10,
}

func (rt *Router) requireRole(w http.ResponseWriter, r *http.Request, minRole auth.Role) bool {
	if !rt.jwtEnabled {
		return true
	}
	claims := auth.ClaimsFromContext(r.Context())
	if claims == nil {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return false
	}
	
	if roleRank[claims.Role] < roleRank[minRole] {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}

func (rt *Router) sendJSON(w http.ResponseWriter, data any) {
	w.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(w).Encode(data); err != nil {
		// The response status has already been written (200 by default), so we
		// can't surface this to the client. Log it for ops visibility — the
		// most common cause is the client closing the connection mid-write.
		logger.Warn("sendJSON: encode response", "error", err)
	}
}

func (rt *Router) sendError(w http.ResponseWriter, err error, code int) {
	if code >= 500 {
		logger.Error("internal server error", "error", err)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	msg := err.Error()
	// Don't leak internal details (database errors, stack paths) to the client for 5xx responses.
	if code >= 500 {
		msg = "internal server error"
	}
	if encErr := json.NewEncoder(w).Encode(map[string]string{"error": msg}); encErr != nil {
		logger.Warn("sendError: encode error response", "error", encErr, "status", code)
	}
}
