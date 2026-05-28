package api

import (
	"encoding/json"
	"net/http"
	"strings"

	"pad-analyzer/internal/auth"
	"pad-analyzer/internal/logger"
)

func (rt *Router) dispatch(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path

	// Library routes have dynamic path segments (/api/library/:id).
	if strings.HasPrefix(path, "/api/library/") {
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
	case "/healthz", "/api/health":
		rt.handleHealth(w, r)

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

	// --- Library ---
	case "/api/library":
		if r.Method == http.MethodGet {
			rt.handleLibraryList(w, r)
		} else if r.Method == http.MethodPost {
			rt.handleLibraryCreate(w, r)
		} else {
			http.NotFound(w, r)
		}

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

			// Serve static files. If the file doesn't exist, serve index.html (SPA support).
			fs := http.Dir(rt.staticDir)
			file, err := fs.Open(path)
			if err != nil {
				// Fallback to index.html for SPA routing
				http.ServeFile(w, r, rt.staticDir+"/index.html")
				return
			}
			file.Close()
			http.FileServer(fs).ServeHTTP(w, r)
			return
		}

		http.NotFound(w, r)
	}
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
	json.NewEncoder(w).Encode(data)
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
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}
