package api

import (
	"net/http"
	"strings"

	"pad-analyzer/internal/api/middleware"
	"github.com/go-chi/chi/v5"
)

func registerRoutes(rt *Router, r chi.Router) {
	// Cross-cutting middleware — applied to every request in order.
	r.Use(rt.securityHeaders)
	r.Use(rt.corsHeaders)
	r.Use(rt.jwtAuth)

	h := rt.handlers

	// --- Swagger UI ---
	r.Get("/swagger", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/swagger/index.html", http.StatusMovedPermanently)
	})
	r.Get("/swagger/*", swaggerHandler())

	// --- WebSocket ---
	r.Get("/ws", rt.handleWebSocket)

	// --- Public / Infrastructure ---
	r.Get("/api/local-config", h.Sys.handleLocalConfig)
	r.Get("/healthz", h.Sys.handleLiveness)
	r.Get("/readyz", h.Sys.handleReadiness)
	r.Get("/api/health", h.Sys.handleReadiness)
	r.Handle("/metrics", middleware.MetricsHandler())
	r.Get("/api/events", rt.eventManager.HandleEvents)

	// --- System & Keys ---
	r.Route("/api/system", func(r chi.Router) {
		r.Get("/settings", h.Sys.handleGetSettings)
		r.Post("/settings", h.Sys.handleUpdateSettings)
		r.Put("/settings", h.Sys.handleUpdateSettings)
		r.Get("/info", h.Sys.handleAppInfo)
		r.Post("/log-error", h.Sys.handleLogError)
	})

	r.Route("/api/keys", func(r chi.Router) {
		r.Post("/save", h.Sys.handleSaveApiKey)
		r.Post("/has", h.Sys.handleHasApiKey)
		r.Post("/delete", h.Sys.handleDeleteApiKey)
	})

	// --- Flow Operations ---
	r.Route("/api/flow", func(r chi.Router) {
		r.Post("/upload", h.Flow.handleUploadFlow)
		r.Post("/load-path", h.Flow.handleLoadFlowFromPath)
		r.Post("/load-folder", h.Flow.handleLoadFlowFolder)
		r.Get("/recent", h.Flow.handleRecentFiles)
		r.Post("/remove-recent", h.Flow.handleRemoveRecentFile)
		r.Post("/clear-recent", h.Flow.handleClearRecentFiles)
		r.Post("/reveal", h.Flow.handleRevealInFileManager)
		r.Post("/search", h.Flow.handleSearchFlow)
		r.Get("/source-files", h.Flow.handleGetSourceFiles)
		r.Post("/read-sources", h.Flow.handleReadSourceFiles)
		r.Post("/open-from-system", h.Flow.handleOnFileOpenFromSystem)
	})

	// --- Library (Cloud CRUD) ---
	r.Route("/api/library", func(r chi.Router) {
		r.Get("/", h.Library.handleLibraryList)
		r.Post("/", h.Library.handleLibraryCreate)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", h.Library.handleLibraryGet)
			r.Put("/", h.Library.handleLibraryUpdate)
			r.Delete("/", h.Library.handleLibraryDelete)
			r.Get("/content", h.Library.handleLibraryGetContent)
		})
	})

	// --- AI Chat ---
	r.Route("/api/chat", func(r chi.Router) {
		r.Post("/stream", h.Chat.handleStreamChatMessage)
		r.Post("/begin", h.Chat.handleBeginStream)
		r.Post("/cancel", h.Chat.handleCancelStream)
		r.Post("/get", h.Chat.handleGetConversation)
		r.Post("/save", h.Chat.handleSaveConversation)
		r.Post("/clear", h.Chat.handleClearConversation)
		r.Post("/export", h.Chat.handleExportConversation)
		r.Get("/demo-remaining", h.Chat.handleGetDemoRemaining)
		r.Post("/preview-context", h.Chat.handlePreviewContext)
		r.Post("/suggested-prompts", h.Chat.handleGetSuggestedPrompts)
	})

	// --- Analysis & Export ---
	r.Route("/api/analysis", func(r chi.Router) {
		r.Post("/analyze", h.Analysis.handleAnalyzeFlow)
		r.Post("/lineage", h.Analysis.handleGetVariableLineage)
		r.Post("/graph", h.Analysis.handleGetExecutionGraph)
		r.Get("/rules", h.Analysis.handleGetRules)
		r.Post("/rule/enabled", h.Analysis.handleSetRuleEnabled)
		r.Post("/rule/config", h.Analysis.handleUpdateRuleConfig)
	})

	r.Route("/api/export", func(r chi.Router) {
		r.Post("/compare", h.Export.handleCompareCurrentWith)
		r.Post("/markdown", h.Export.handleExportMarkdown)
		r.Post("/pdf", h.Export.handleExportPDF)
	})

	// --- Providers & Auth ---
	r.Route("/api/providers", func(r chi.Router) {
		r.Get("/list", h.Provider.handleListProviders)
		r.Post("/test", h.Provider.handleTestProviderConnection)
		r.Route("/github", func(r chi.Router) {
			r.Post("/start", h.Provider.handleStartGitHubAuth)
			r.Post("/poll", h.Provider.handlePollGitHubAuth)
			r.Post("/revoke", h.Provider.handleRevokeGitHubAuth)
			r.Get("/user", h.Provider.handleGetGitHubUser)
		})
		r.Route("/copilot", func(r chi.Router) {
			r.Post("/start", h.Provider.handleStartCopilotAuth)
			r.Post("/poll", h.Provider.handlePollCopilotAuth)
			r.Post("/revoke", h.Provider.handleRevokeCopilotAuth)
			r.Get("/user", h.Provider.handleGetCopilotUser)
		})
	})

	r.Route("/api/auth", func(r chi.Router) {
		r.Post("/register", h.Auth.handleAuthRegister)
		r.Post("/login", h.Auth.handleAuthLogin)
		r.Post("/refresh", h.Auth.handleAuthRefresh)
		r.Get("/me", h.Auth.handleAuthMe)
		r.Post("/logout", h.Auth.handleAuthLogout)
		r.Post("/change-password", h.Auth.handleAuthChangePassword)
	})
	r.Post("/api/ws-ticket", h.Auth.handleWSTicket)

	// --- Organizations & Sharing ---
	r.Route("/api/orgs", func(r chi.Router) {
		r.Get("/", h.Org.handleOrgList)
		r.Post("/", h.Org.handleOrgCreate)
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", h.Org.handleOrgGet)
			r.Put("/", h.Org.handleOrgUpdate)
			r.Delete("/", h.Org.handleOrgDelete)
			r.Route("/members", func(r chi.Router) {
				r.Get("/", h.Org.handleOrgMemberList)
				r.Post("/", h.Org.handleOrgMemberAdd)
				r.Route("/{userId}", func(r chi.Router) {
					r.Delete("/", h.Org.handleOrgMemberRemove)
					r.Put("/role", h.Org.handleOrgMemberRoleUpdate)
				})
			})
		})
	})

	r.Route("/api/flows/{flowId}/collaborators", func(r chi.Router) {
		r.Get("/", h.Sharing.handleCollaboratorList)
		r.Post("/", h.Sharing.handleCollaboratorAdd)
		r.Route("/{userId}", func(r chi.Router) {
			r.Delete("/", h.Sharing.handleCollaboratorRemove)
			r.Put("/", h.Sharing.handleCollaboratorUpdate)
		})
	})

	// --- Admin ---
	r.Route("/api/admin", func(r chi.Router) {
		r.Get("/users/list", h.Admin.handleAdminUserList)
		r.Put("/users/{id}/role", h.Admin.handleAdminUserRole)
		r.Post("/migration/start", h.Admin.handleMigrationStart)
		r.Get("/migration/status", h.Admin.handleMigrationStatus)
	})

	r.NotFound(func(w http.ResponseWriter, r *http.Request) {
		importPath := r.URL.Path
		if rt.staticDir != "" {
			if strings.HasPrefix(importPath, "/api/") {
				http.NotFound(w, r)
				return
			}
			rt.serveStatic(w, r, importPath)
			return
		}
		http.NotFound(w, r)
	})
}
