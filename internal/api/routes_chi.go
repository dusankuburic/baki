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

// registerRoutes applies chi's inner middleware layer and the full route table; it runs after the
// outer layers built by BuildHandler (middleware_chain.go — see that file's doc for the resolved order).
func registerRoutes(rt *Router, r chi.Router) {
	// Cross-cutting middleware — applied to every request in order.
	r.Use(rt.securityHeaders)
	r.Use(rt.corsHeaders)

	// API version alias: the rewrite must stay identical to canonicalRequestPath (middleware_chain.go),
	// which the rate-limit classifier runs outside this mux — the copies drifted once into a bypass.
	// chi routes on RawPath, so strip it there too or an escaped "/" in an id 404s.
	r.Use(func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
			applyVersionAlias(req.URL)
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
	// Always registered; in cloud mode the handler checks for a valid JWT so the schema isn't
	// browsable unauthenticated. In local mode, open access.
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
	// /metrics: private-IP allowlist alone passes for every request behind a reverse proxy — also set
	// PAD_METRICS_TOKEN and NSG/ACA ingress rules (warnIfMetricsExposed logs that combination at boot).
	r.Handle("/metrics", middleware.MetricsGuard(rt.trustedProxies, rt.metricsToken)(middleware.MetricsHandler()))
	// /debug/pprof/*: OPT-IN (PAD_PPROF_ENABLED) — it used to sit unconditionally behind the private-IP
	// guard, making /debug/pprof/heap a public heap dump behind a proxy; validation refuses the flag
	// without a token.
	if rt.pprofEnabled {
		// A dedicated ServeMux so the handlers don't leak onto
		// http.DefaultServeMux (which other imports might accidentally expose).
		pprofMux := http.NewServeMux()
		pprofMux.HandleFunc("/debug/pprof/", pprof.Index)
		pprofMux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
		pprofMux.HandleFunc("/debug/pprof/profile", pprof.Profile)
		pprofMux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
		pprofMux.HandleFunc("/debug/pprof/trace", pprof.Trace)
		r.Handle("/debug/pprof/*", middleware.MetricsGuard(rt.trustedProxies, rt.metricsToken)(pprofMux))
	}
	r.Get("/api/events", rt.eventManager.HandleEvents)

	// --- System & Keys ---
	h.Sys.registerRoutes(r)

	// --- Flow Operations ---
	h.Flow.registerRoutes(r)
	r.Get("/api/shared", h.Analysis.handleViewShared)          // unauthenticated: ?token=...
	r.Post("/api/integrations/ci", h.Analysis.handleCIWebhook) // HMAC-authenticated (X-Baki-Signature)

	// --- Library (Cloud CRUD) ---
	h.Library.registerRoutes(r)

	// --- AI Chat ---
	h.Chat.registerRoutes(r)

	// --- Analysis ---
	h.Analysis.registerRoutes(r)

	// --- Welcome Dashboard (BFF) ---
	h.Dashboard.registerRoutes(r)

	// --- Governance alerts inbox (notifications bell) ---
	h.Analysis.registerGovernanceRoutes(r)

	h.Export.registerRoutes(r)

	// --- Providers & Auth ---
	h.Provider.registerRoutes(r)

	h.Auth.registerRoutes(r)

	// --- Organizations & Sharing ---
	h.Org.registerRoutes(r)

	h.Sharing.registerRoutes(r)

	// --- Admin ---
	h.Admin.registerRoutes(r, h.Sys)

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
