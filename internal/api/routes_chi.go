package api

import (
	"fmt"
	"net/http"
	"net/http/pprof"
	"strings"

	"github.com/go-chi/chi/v5"
	"pad-analyzer/internal/api/middleware"
	"pad-analyzer/internal/api/render"
)

// registerRoutes applies chi's inner middleware layer (below) and the full
// route table. It runs after the outer layers built by BuildHandler
// (middleware_chain.go, same package) — see that file's doc comment for the
// complete, resolved end-to-end order.
func registerRoutes(rt *Router, r chi.Router) {
	// Cross-cutting middleware — applied to every request in order.
	r.Use(rt.securityHeaders)
	r.Use(rt.corsHeaders)

	// API version alias: transparently rewrite /api/v1/... → /api/... so new
	// clients can target /api/v1/ while existing clients keep using /api/.
	// Future breaking changes land on /api/v2/ only; /api/ stays backward-
	// compatible. The rewrite happens before jwtAuth and routing so the rest
	// of the pipeline is unaware of the version prefix.
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			if rest := strings.TrimPrefix(req.URL.Path, "/api/v1/"); rest != req.URL.Path {
				req.URL.Path = "/api/" + rest
			} else if rest := strings.TrimPrefix(req.URL.Path, "/api/v1"); rest == "" {
				req.URL.Path = "/api"
			}
			next.ServeHTTP(w, req)
		})
	})

	r.Use(rt.jwtAuth)
	// Per-user write throttle (cloud mode only; pass-through when the limiter is
	// nil). Runs after jwtAuth so claims are available, and before rlsMiddleware.
	r.Use(rt.perUserRateLimit)
	r.Use(rt.rlsMiddleware)

	h := rt.handlers

	// --- Swagger UI ---
	// Always registered. In cloud mode (JWTEnabled), the swagger handler
	// checks for a valid JWT so the schema isn't browsable unauthenticated.
	// In local mode, open access (same as before).
	swaggerH := swaggerHandler()
	r.Get("/swagger", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, "/swagger/index.html", http.StatusMovedPermanently)
	})
	r.Get("/swagger/*", func(w http.ResponseWriter, r *http.Request) {
		if rt.security.JWTEnabled {
			tokenStr := strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			if tokenStr == "" {
				render.Error(w, fmt.Errorf("unauthorized"), http.StatusUnauthorized)
				return
			}
		}
		swaggerH.ServeHTTP(w, r)
	})

	// --- WebSocket ---
	r.Get("/ws", rt.handleWebSocket)

	// --- Public / Infrastructure ---
	r.Get("/api/local-config", h.Sys.handleLocalConfig)
	r.Get("/api/system/features", h.Sys.handleFeatures) // public: pre-auth flag read (login page)
	r.Get("/healthz", h.Sys.handleLiveness)
	r.Get("/readyz", h.Sys.handleReadiness)
	r.Get("/api/health", h.Sys.handleReadiness)
	// /metrics is protected by a private-IP allowlist to prevent public internet
	// access to internal operational data. Supplement this with Azure NSG /
	// ACA ingress rules that block /metrics from the public load balancer.
	r.Handle("/metrics", middleware.MetricsGuard(rt.trustedProxies, rt.metricsToken)(middleware.MetricsHandler()))
	// /debug/pprof/* — Go runtime profiling, same private-IP guard as /metrics.
	// Register the pprof handlers on a dedicated ServeMux so they don't leak
	// onto http.DefaultServeMux (which other imports might accidentally expose).
	pprofMux := http.NewServeMux()
	pprofMux.HandleFunc("/debug/pprof/", pprof.Index)
	pprofMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	pprofMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	pprofMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	pprofMux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	r.Handle("/debug/pprof/*", middleware.MetricsGuard(rt.trustedProxies, rt.metricsToken)(pprofMux))
	r.Get("/api/events", rt.eventManager.HandleEvents)

	// --- System & Keys ---
	r.Route("/api/system", func(r chi.Router) {
		r.Get("/settings", h.Sys.handleGetSettings)
		r.Post("/settings", h.Sys.handleUpdateSettings)
		r.Put("/settings", h.Sys.handleUpdateSettings)
		r.Get("/settings/user", h.Sys.handleGetSettings)
		r.Post("/settings/user", h.Sys.handleUpdateSettings)
		r.Get("/settings/org/{id}", h.Sys.handleGetOrgSettings)     // member-only
		r.Post("/settings/org/{id}", h.Sys.handleUpdateOrgSettings) // admin-only
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
		r.Post("/search-library", h.Flow.handleSearchLibrary)
		r.Get("/source-files", h.Flow.handleGetSourceFiles)
		r.Post("/read-sources", h.Flow.handleReadSourceFiles)
		r.Post("/open-from-system", h.Flow.handleOnFileOpenFromSystem)
		r.Post("/suppress-in-source", h.Flow.handleSuppressInSource)
		r.Post("/apply-fix", h.Flow.handleApplyFix)
		r.Post("/apply-fix-batch", h.Flow.handleApplyFixBatch)
		r.Post("/save-source", h.Flow.handleSaveSource)
		r.Get("/source", h.Flow.handleGetSource)
		r.Post("/preview-fix", h.Flow.handlePreviewFix)
		r.Post("/reimport", h.Flow.handleReimport)
		// Share links (read-only public report)
		r.Post("/share/create", h.Flow.handleCreateShare)
		r.Post("/share/list", h.Flow.handleListShares)
		r.Post("/share/revoke", h.Flow.handleRevokeShare)
	})
	r.Get("/api/shared", h.Analysis.handleViewShared)          // unauthenticated: ?token=...
	r.Post("/api/integrations/ci", h.Analysis.handleCIWebhook) // HMAC-authenticated (X-Baki-Signature)

	// --- Library (Cloud CRUD) ---
	r.Route("/api/library", func(r chi.Router) {
		r.Get("/", h.Library.handleLibraryList)
		r.Post("/", h.Library.handleLibraryCreate)
		r.Get("/portfolio", h.Library.handlePortfolio) // org-wide governance fleet view
		r.Route("/{id}", func(r chi.Router) {
			r.Get("/", h.Library.handleLibraryGet)
			r.Put("/", h.Library.handleLibraryUpdate)
			r.Delete("/", h.Library.handleLibraryDelete)
			r.Get("/content", h.Library.handleLibraryGetContent)
			r.Route("/versions", func(r chi.Router) {
				r.Get("/", h.Library.handleListFlowVersions)
				r.Post("/", h.Library.handleSaveFlowVersion)
				r.Get("/{vn}", h.Library.handleGetFlowVersion)
				r.Post("/{vn}/restore", h.Library.handleRestoreFlowVersion)
				r.Get("/{vn}/diff", h.Library.handleDiffFlowVersion)
			})
		})
	})

	// --- AI Chat ---
	r.Route("/api/chat", func(r chi.Router) {
		r.Post("/stream", h.Chat.handleStreamChatMessage)
		r.Post("/begin", h.Chat.handleBeginStream)
		r.Post("/cancel", h.Chat.handleCancelStream)
		r.Post("/resume", h.Chat.handleResumeStream)
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
		r.Post("/analyze-raw", h.Analysis.handleAnalyzeRaw)
		r.Post("/lineage", h.Analysis.handleGetVariableLineage)
		r.Post("/graph", h.Analysis.handleGetExecutionGraph)
		r.Post("/metrics", h.Analysis.handleGetMetrics)
		r.Post("/dataflow", h.Analysis.handleGetDataFlow)
		r.Post("/batch", h.Analysis.handleBatchAnalyze)
		r.Post("/diff", h.Analysis.handleDiff)
		r.Post("/history", h.Analysis.handleGetHistory)
		r.Post("/export/sarif", h.Analysis.handleExportSARIF)
		r.Post("/export/junit", h.Analysis.handleExportJUnit)
		r.Post("/export/csv", h.Analysis.handleExportCSV)
		r.Get("/dependencies", h.Analysis.handleGetDependencies)
		r.Get("/dashboard", h.Analysis.handleGetDashboard)
		r.Post("/subflow-hashes", h.Analysis.handleGetSubflowHashes)
		r.Post("/deduplicate", h.Analysis.handleDeduplicate)
		r.Post("/related", h.Analysis.handleRelatedFindings)
		r.Post("/compare", h.Analysis.handleCompareFlows)
		r.Get("/rules", h.Analysis.handleGetRules)
		r.Get("/rules/summary", h.Analysis.handleGetRulesSummary)
		r.Post("/rule/enabled", h.Analysis.handleSetRuleEnabled)
		r.Post("/rule/config", h.Analysis.handleUpdateRuleConfig)
		// Finding triage & baselines (persistent, team-shared; cloud mode only)
		r.Post("/triage/list", h.Analysis.handleListFindingStatuses)
		r.Post("/triage/set", h.Analysis.handleSetFindingStatus)
		r.Post("/triage/set-batch", h.Analysis.handleBatchSetFindingStatus)
		r.Post("/triage/clear", h.Analysis.handleClearFindingStatus)
		r.Post("/baseline/get", h.Analysis.handleGetBaseline)
		r.Post("/baseline/set", h.Analysis.handleSetBaseline)
		r.Post("/baseline/clear", h.Analysis.handleClearBaseline)
		r.Post("/baseline/drift", h.Analysis.handleBaselineDrift)
		r.Post("/policy/evaluate", h.Analysis.handleEvaluatePolicy)           // gate a flow against an inline policy
		r.Post("/policy/save", h.Analysis.handleSavePolicy)                   // persist a named policy (cloud)
		r.Post("/policy/list", h.Analysis.handleListPolicies)                 // list org's policies (cloud)
		r.Post("/policy/get", h.Analysis.handleGetPolicy)                     // get a single policy (cloud)
		r.Post("/policy/delete", h.Analysis.handleDeletePolicy)               // delete a policy (cloud)
		r.Post("/policy/evaluate-by-id", h.Analysis.handleEvaluatePolicyByID) // gate a flow against a persisted policy
		// Finding comments (team-shared review threads)
		r.Post("/comments/list", h.Analysis.handleListComments)
		r.Post("/comments/add", h.Analysis.handleAddComment)
		r.Post("/comments/delete", h.Analysis.handleDeleteComment)
	})

	// --- Welcome Dashboard (BFF) ---
	r.Get("/api/dashboard/home", h.Dashboard.handleHome)

	// --- Governance alerts inbox (notifications bell) ---
	// Scanner-written drift/regression alerts; RLS-scoped to the caller's visible
	// flows. All read/write (no jwtAuth bypass needed).
	r.Route("/api/governance", func(r chi.Router) {
		r.Get("/alerts", h.Analysis.handleListGovernanceAlerts)
		r.Get("/alerts/unread-count", h.Analysis.handleUnreadGovernanceAlertCount)
		r.Post("/alerts/read", h.Analysis.handleMarkGovernanceAlertRead)
		r.Post("/alerts/read-all", h.Analysis.handleMarkAllGovernanceAlertsRead)
		r.Post("/alerts/dismiss", h.Analysis.handleDismissGovernanceAlert)
		r.Delete("/alerts", h.Analysis.handleClearGovernanceAlerts)
	})

	r.Route("/api/export", func(r chi.Router) {
		r.Post("/compare", h.Export.handleCompareCurrentWith)
		r.Post("/markdown", h.Export.handleExportMarkdown)
		r.Post("/html", h.Export.handleExportHTML)
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
		r.Post("/forgot-password", h.Auth.handleAuthForgotPassword)
		r.Post("/reset-password", h.Auth.handleAuthResetPassword)
		r.Post("/verify-email", h.Auth.handleAuthVerifyEmail)
		r.Get("/me", h.Auth.handleAuthMe)
		r.Put("/profile", h.Auth.handleAuthUpdateProfile)
		r.Post("/logout", h.Auth.handleAuthLogout)
		r.Post("/change-password", h.Auth.handleAuthChangePassword)
		r.Get("/sessions", h.Auth.handleAuthSessions)
		r.Delete("/sessions/{id}", h.Auth.handleAuthSessionRevoke)
		// Self-service account erasure (GDPR) + data-subject export.
		r.Delete("/account", h.Auth.handleAuthDeleteAccount)
		r.Get("/account/export", h.Auth.handleAuthExportAccount)
		// Machine API tokens (PATs) for headless/CI access (cloud mode)
		r.Get("/tokens", h.Auth.handleListAPITokens)
		r.Post("/tokens", h.Auth.handleCreateAPIToken)
		r.Delete("/tokens/{id}", h.Auth.handleDeleteAPIToken)
		r.Route("/sso", func(r chi.Router) {
			r.Get("/info", h.Auth.handleSSOInfo)
			r.Get("/start", h.Auth.handleSSOStart)
			r.Get("/callback", h.Auth.handleSSOCallback)
			r.Post("/exchange", h.Auth.handleSSOExchange)
		})
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
			r.Route("/knowledge", func(r chi.Router) {
				r.Get("/", h.Org.handleKnowledgeList)
				r.Post("/upload", h.Org.handleKnowledgeUpload)
				r.Delete("/{docId}", h.Org.handleKnowledgeDelete)
			})
			r.Route("/invites", func(r chi.Router) {
				r.Get("/", h.Org.handleOrgInviteList)
				r.Post("/", h.Org.handleOrgInviteCreate)
				r.Delete("/{inviteId}", h.Org.handleOrgInviteRevoke)
			})
		})
	})

	r.Route("/api/invites", func(r chi.Router) {
		r.Post("/{token}/accept", h.Org.handleInviteAccept)
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
		r.Get("/audit", h.Admin.handleAdminAuditList)
		r.Post("/powerplatform/start", h.Admin.handlePPStartAuth)
		r.Post("/powerplatform/poll", h.Admin.handlePPPollAuth)
		r.Get("/powerplatform/status", h.Admin.handlePPStatus)
		r.Post("/scanner/scan", h.Admin.handleScannerScan)
		r.Post("/ingester/ingest", h.Admin.handleIngesterIngest)
		r.Get("/system/health", h.Sys.handleAdminSystemHealth)
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
